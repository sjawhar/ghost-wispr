package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// jabraStream is the speaker's playback stream on the Jabra, playing in real
// time at 48 kHz. open is true from when it opens until it closes, and
// closedAt is when it closed.
type jabraStream struct {
	open     atomic.Bool
	mu       sync.Mutex
	closedAt time.Time
}

func (s *jabraStream) Write(frame []int16) error {
	time.Sleep(time.Duration(len(frame)) * time.Second / 48000)
	return nil
}

func (s *jabraStream) Drop() (int, error) { return 0, nil }

func (s *jabraStream) Close() error {
	s.mu.Lock()
	s.closedAt = time.Now()
	s.mu.Unlock()
	s.open.Store(false)
	return nil
}

func (s *jabraStream) closed() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closedAt
}

// outputOnDevice makes the speaker's Output wired to device as main wires
// it, with its default idle close, on a jabraStream.
func outputOnDevice(t *testing.T, device *captureDevice) (*audio.Output, *jabraStream) {
	t.Helper()
	stream := &jabraStream{}
	out := audio.NewOutput(audio.OutputConfig{
		Device:  "J75",
		Opening: device.playbackOpening,
		Closing: device.playbackClosing,
		Open: func(string) (audio.PlaybackStream, int, error) {
			stream.open.Store(true)
			return stream, 48000, nil
		},
	})
	t.Cleanup(func() { _ = out.Close() })
	return out, stream
}

// speakOn plays d of a tone on out's voice input and returns once it has all
// played.
func speakOn(t *testing.T, out *audio.Output, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	samples := make([]byte, 2*48000*int(d/time.Millisecond)/1000)
	for i := range samples {
		samples[i] = byte(i)
	}
	pb := out.Begin(audio.InputVoice)
	if err := pb.WriteAll(ctx, samples, audio.AudioFormat{SampleRate: 48000, Channels: 1}); err != nil {
		t.Fatal(err)
	}
	if err := pb.Drain(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestAResumeWhileTheOutputIsOpenWaitsForItsClose fails if a resume opens the
// microphone, or confirms the recording light red, while the speaker's Output
// holds its stream open on the Jabra, also between two sounds: the light
// cannot change while a stream is open, so the ring would read Muted while
// the unit records. The resume waits for the stream to close.
func TestAResumeWhileTheOutputIsOpenWaitsForItsClose(t *testing.T) {
	r := newSpeechResumeRig(t)
	mic := r.startMicLoop(t)
	out, stream := outputOnDevice(t, r.device)
	speakOn(t, out, 40*time.Millisecond)
	if !stream.open.Load() {
		t.Fatal("control: the output's stream is not open just after its sound played")
	}

	r.capture.Resume()
	waitOpen(t, r.capture)
	opensDuring(t, mic, 100*time.Millisecond, "while the output's stream was open between sounds")
	if got := r.reds.Load(); got != 0 {
		t.Fatalf("the recording light was confirmed red %d times while the output's stream was open", got)
	}
	if got := r.capture.captureState(fakeMicState(false)); got != captureResuming {
		t.Fatalf("capture state while the resume waits for the output = %q, want %q", got, captureResuming)
	}
	speakOn(t, out, 100*time.Millisecond) // a second sound keeps the stream open
	mic.expectEvents(t, "open")
	if stream.open.Load() {
		t.Fatal("the microphone opened while the output's stream was open")
	}
	if r.ring.Load() {
		t.Fatal("the microphone opened while the Jabra's ring showed Muted")
	}
}

// TestAnOutputIdle300msClosesAndTheResumeProceeds fails if the speaker's
// Output keeps its stream open on the Jabra much past 300 ms with nothing to
// play, or closes it sooner, or if a resume that waited for it does not
// proceed once it has closed.
func TestAnOutputIdle300msClosesAndTheResumeProceeds(t *testing.T) {
	r := newSpeechResumeRig(t)
	mic := r.startMicLoop(t)
	out, stream := outputOnDevice(t, r.device)
	speakOn(t, out, 40*time.Millisecond)
	played := time.Now()
	r.capture.Resume()
	mic.expectEvents(t, "open")
	opened := time.Now()
	if stream.open.Load() {
		t.Fatal("the microphone opened while the output's stream was open")
	}
	idle := stream.closed().Sub(played)
	if idle < 300*time.Millisecond || idle > 400*time.Millisecond {
		t.Fatalf("the output closed its stream %v after its last sound, want 300 ms", idle)
	}
	if opened.Before(stream.closed()) {
		t.Fatal("the microphone opened before the output closed its stream")
	}
	if r.ring.Load() {
		t.Fatal("the microphone opened while the Jabra's ring showed Muted")
	}
}
