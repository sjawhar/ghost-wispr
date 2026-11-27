package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/recording"
)

// heldAudioRig is the app's capture gate (newSessionCaptureGate) in front of
// the real recorder, which keeps a minute of audio from before a session, as
// it does with live transcription on. lightOffs counts the times the light
// read back as anything but red; micClosed gets a signal after each close of
// the microphone has run the gate's close steps.
type heldAudioRig struct {
	dir       string
	recorder  *audio.Recorder
	capture   *captureGate
	lightOffs atomic.Uint64
	micClosed chan struct{}
}

func newHeldAudioRig(t *testing.T) *heldAudioRig {
	t.Helper()
	r := &heldAudioRig{dir: t.TempDir(), micClosed: make(chan struct{}, 8)}
	r.recorder = audio.NewRecorder(r.dir)
	r.recorder.SetLookback(time.Minute)
	r.capture = newSessionCaptureGate(context.Background(), &fakeSessions{log: newCallLog()}, r.recorder, func() deepgramFinalizer { return nil }, time.Hour, testEndRetry, nopLogf)
	r.capture.lightOffs = r.lightOffs.Load
	r.capture.lightRedMark = r.lightOffs.Load
	r.capture.afterMicClose = func() {
		select {
		case r.micClosed <- struct{}{}:
		default:
		}
	}
	return r
}

// nextRecording returns the labels of the seconds a recording that started
// now would open with, if Deepgram had just heard words from a minute ago.
func (r *heldAudioRig) nextRecording(t *testing.T) []uint16 {
	t.Helper()
	r.recorder.HoldPreroll(time.Minute)
	const id = "20261119090000"
	if err := r.recorder.StartSession(id); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	return recordedLabels(t, recording.RawPath(r.dir, id))
}

// TestAudioFromBeforeTheLightReadOffNeverLeadsARecording fails if audio
// captured before the recording light read back off leads a recording that
// starts after it reads red again. The microphone closes and opens again
// under a fresh red confirmation, but the recorder kept the audio it held
// (up to a minute, plus any hold) across the dark moment.
func TestAudioFromBeforeTheLightReadOffNeverLeadsARecording(t *testing.T) {
	r := newHeldAudioRig(t)
	mic := newScriptedMic()
	runMicLoop(t, r.capture, mic, r.recorder.Writer(io.Discard), func(time.Duration) {}, nil)
	mic.expectEvents(t, "stream")
	mic.capture(t, reconnectSecond(1)) // captured while red
	r.lightOffs.Add(1)                 // the light reads back off
	mic.capture(t, reconnectSecond(2)) // the gate drops it; the loop closes the microphone
	mic.expectEvents(t, "close", "open", "stream")
	mic.capture(t, reconnectSecond(3)) // red again

	if got, want := r.nextRecording(t), []uint16{3}; !slices.Equal(got, want) {
		t.Fatalf("a recording started after the light read red again opens with seconds %v; want %v, nothing from before the light read off", got, want)
	}
}

// TestAudioFromBeforeAPauseIsDroppedWhenTheMicrophoneCloses fails if the
// recorder still holds audio from before a pause once the pause has closed
// the microphone: up to a minute of the room's audio, and any hold, stayed in
// memory for the whole pause, where before only the last 5 s did.
func TestAudioFromBeforeAPauseIsDroppedWhenTheMicrophoneCloses(t *testing.T) {
	r := newHeldAudioRig(t)
	mic := newScriptedMic()
	runMicLoop(t, r.capture, mic, r.recorder.Writer(io.Discard), func(time.Duration) {}, nil)
	mic.expectEvents(t, "stream")
	mic.capture(t, reconnectSecond(1))
	r.recorder.HoldPreroll(0)
	mic.capture(t, reconnectSecond(2))
	r.capture.Pause()
	mic.capture(t, reconnectSecond(3)) // the device's next buffer, which the gate drops
	mic.expectEvents(t, "close")
	<-r.micClosed

	if got := r.nextRecording(t); len(got) != 0 {
		t.Fatalf("while paused, the recorder holds seconds %v from before the pause; want none once the microphone closed", got)
	}
}

// stallingMic is a capture device whose Stream writes the chunks the test
// sends until Close, which ends it as the real device's Close does.
type stallingMic struct {
	chunks chan []byte
	events chan string
	mu     sync.Mutex
	closed chan struct{}
}

func newStallingMic() *stallingMic {
	return &stallingMic{chunks: make(chan []byte), events: make(chan string, 32), closed: make(chan struct{})}
}

func (m *stallingMic) Stream(w io.Writer) error {
	m.events <- "stream"
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	for {
		select {
		case chunk := <-m.chunks:
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		case <-closed:
			return errors.New("stream closed")
		}
	}
}

func (m *stallingMic) Close() error {
	m.events <- "close"
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return nil
}

func (m *stallingMic) open(event string) error {
	m.events <- event
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = make(chan struct{})
	return nil
}

func (m *stallingMic) Start() error  { return m.open("start") }
func (m *stallingMic) Open() error   { return m.open("open") }
func (m *stallingMic) Reopen() error { return m.open("reopen") }

func (m *stallingMic) expect(t *testing.T, want ...string) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-m.events:
			if got != w {
				t.Fatalf("device call %q, want %q (want %v)", got, w, want)
			}
		case <-time.After(testWait):
			t.Fatalf("timed out waiting for device call %q (want %v)", w, want)
		}
	}
}

// TestAudioFromBeforeAMicStallNeverLeadsARecording fails if audio captured
// before a stall, when no audio reached the recording and the microphone
// closed and the light went off with it, leads a recording that starts after
// the microphone opens again under red.
func TestAudioFromBeforeAMicStallNeverLeadsARecording(t *testing.T) {
	runParallel(t)
	r := newHeldAudioRig(t)
	r.capture.audioStall = 300 * time.Millisecond
	r.capture.finishMicStart() // the start arms the stall timer
	mic := newStallingMic()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamMicWithRetry(ctx, r.capture, mic, r.recorder.Writer(io.Discard), func(time.Duration) {}, nopLogf, nil)
	}()
	t.Cleanup(func() {
		cancel()
		_ = mic.Close()
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("mic loop did not stop")
		}
	})
	mic.expect(t, "stream")
	mic.chunks <- reconnectSecond(1)
	mic.expect(t, "close", "open", "stream") // no audio for 300 ms: the stall
	mic.chunks <- reconnectSecond(3)
	// The chunk is written once the loop takes the next one, or closes.
	mic.chunks <- nil

	if got, want := r.nextRecording(t), []uint16{3}; !slices.Equal(got, want) {
		t.Fatalf("a recording started after the stall opens with seconds %v; want %v, nothing from before it", got, want)
	}
}
