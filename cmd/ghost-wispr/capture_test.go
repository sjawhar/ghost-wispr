package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// testWait bounds every wait for the code under test; generous for go test -race on a Pi.
const testWait = 10 * time.Second

// testEndRetry is the first wait before a pause retries a session end that
// failed.
const testEndRetry = time.Millisecond

// scriptedMic is a fake capture device for streamMicWithRetry. Stream calls
// onStream once if it is set, returns the next scripted error if any, and
// otherwise writes each chunk the test sends on chunks and reports the write's
// result on written. Every call to Stream, Start, Open, Close and Reopen is
// reported on events, in order.
type scriptedMic struct {
	chunks     chan []byte
	written    chan error
	events     chan string
	onStream   func()
	streamErrs []error
	startErrs  []error
	openErrs   []error
}

func newScriptedMic() *scriptedMic {
	return &scriptedMic{
		chunks:  make(chan []byte),
		written: make(chan error),
		events:  make(chan string, 32),
	}
}

func (m *scriptedMic) Stream(w io.Writer) error {
	m.events <- "stream"
	if m.onStream != nil {
		onStream := m.onStream
		m.onStream = nil
		onStream()
	}
	if len(m.streamErrs) > 0 {
		err := m.streamErrs[0]
		m.streamErrs = m.streamErrs[1:]
		return err
	}
	for chunk := range m.chunks {
		_, err := w.Write(chunk)
		m.written <- err
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *scriptedMic) Start() error {
	m.events <- "start"
	if len(m.startErrs) > 0 {
		err := m.startErrs[0]
		m.startErrs = m.startErrs[1:]
		return err
	}
	return nil
}

func (m *scriptedMic) Open() error {
	m.events <- "open"
	if len(m.openErrs) > 0 {
		err := m.openErrs[0]
		m.openErrs = m.openErrs[1:]
		return err
	}
	return nil
}

func (m *scriptedMic) Close() error {
	m.events <- "close"
	return nil
}

func (m *scriptedMic) Reopen() error {
	m.events <- "reopen"
	return nil
}

// expectEvents fails the test unless the next device calls are exactly want.
func (m *scriptedMic) expectEvents(t *testing.T, want ...string) {
	t.Helper()
	for i, w := range want {
		select {
		case got := <-m.events:
			if got != w {
				t.Fatalf("device call %d = %q, want %q (want sequence %v)", i+1, got, w, want)
			}
		case <-time.After(testWait):
			t.Fatalf("timed out waiting for device call %d %q (want sequence %v)", i+1, w, want)
		}
	}
}

// expectNoEvents fails the test if the device is called within a short wait.
func (m *scriptedMic) expectNoEvents(t *testing.T) {
	t.Helper()
	select {
	case got := <-m.events:
		t.Fatalf("device call %q while paused, want none", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// capture sends one chunk through the running mic loop and waits until the
// loop has written it.
func (m *scriptedMic) capture(t *testing.T, chunk []byte) {
	t.Helper()
	select {
	case m.chunks <- chunk:
	case <-time.After(testWait):
		t.Fatal("timed out handing a chunk to the mic loop")
	}
	select {
	case <-m.written:
	case <-time.After(testWait):
		t.Fatal("timed out waiting for the mic loop to write a chunk")
	}
}

// lockedBuffer stands in for the Deepgram writer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// recordedWaits records the mic loop's backoff waits; wait may run extra work.
type recordedWaits struct {
	mu    sync.Mutex
	waits []time.Duration
	then  func()
}

func (r *recordedWaits) wait(d time.Duration) {
	r.mu.Lock()
	r.waits = append(r.waits, d)
	then := r.then
	r.mu.Unlock()
	if then != nil {
		then()
	}
}

func (r *recordedWaits) get() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.waits)
}

// runMicLoop starts streamMicWithRetry on mic. It returns a function that
// stops the loop and reports whether it exited; the test's cleanup calls it too.
func runMicLoop(t *testing.T, capture *captureGate, mic *scriptedMic, writer io.Writer, wait func(time.Duration), hub *server.Hub) (stop func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamMicWithRetry(ctx, capture, mic, writer, wait, func(string, ...any) {}, hub)
	}()
	var once sync.Once
	stop = func() bool {
		once.Do(func() {
			cancel()
			close(mic.chunks)
		})
		select {
		case <-done:
			return true
		case <-time.After(testWait):
			return false
		}
	}
	t.Cleanup(func() {
		if !stop() {
			t.Error("mic loop did not stop")
		}
	})
	return stop
}

func fileSize(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return int(info.Size())
}

// The defect: POST /api/pause set a flag that nothing in the audio path read,
// so a paused device kept writing the session recording and streaming to
// Deepgram. While paused, no captured audio may reach either; resume restores
// both.
func TestPausedMicSendsNoAudioToRecorderOrDeepgram(t *testing.T) {
	dir := t.TempDir()
	recorder := audio.NewRecorder(dir)
	if err := recorder.StartSession("meeting"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	t.Cleanup(func() { _, _ = recorder.EndSession("meeting") })
	recording := filepath.Join(dir, "meeting.pcm")
	deepgram := &lockedBuffer{}
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()

	runMicLoop(t, capture, mic, recorder.Writer(deepgram), func(time.Duration) {}, nil)
	mic.expectEvents(t, "stream")

	mic.capture(t, make([]byte, 100))
	if got := deepgram.Len(); got != 100 {
		t.Fatalf("before pause: Deepgram got %d bytes, want 100", got)
	}
	if got := fileSize(t, recording); got != 100 {
		t.Fatalf("before pause: recording has %d bytes, want 100", got)
	}

	capture.Pause()
	mic.capture(t, make([]byte, 100))
	if got := deepgram.Len(); got != 100 {
		t.Fatalf("while paused: Deepgram got %d bytes in total, want 100 (audio still streams while paused)", got)
	}
	if got := fileSize(t, recording); got != 100 {
		t.Fatalf("while paused: recording has %d bytes, want 100 (audio still recorded while paused)", got)
	}

	mic.expectEvents(t, "close")

	capture.Resume()
	mic.expectEvents(t, "open", "stream")
	mic.capture(t, make([]byte, 50))
	if got := deepgram.Len(); got != 150 {
		t.Fatalf("after resume: Deepgram got %d bytes in total, want 150", got)
	}
	if got := fileSize(t, recording); got != 150 {
		t.Fatalf("after resume: recording has %d bytes, want 150", got)
	}
}

// Pause closes the capture device itself, not only the audio after it,
// without treating the pause as a device error; resume opens the device again.
func TestPauseClosesMicDeviceUntilResume(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()
	deepgram := &lockedBuffer{}
	waits := &recordedWaits{}

	runMicLoop(t, capture, mic, deepgram, waits.wait, nil)
	mic.expectEvents(t, "stream")

	capture.Pause()
	mic.capture(t, []byte{1, 2, 3, 4})
	mic.expectEvents(t, "close")
	mic.expectNoEvents(t)

	capture.Resume()
	mic.expectEvents(t, "open", "stream")
	mic.capture(t, []byte{5, 6})
	if got := deepgram.Len(); got != 2 {
		t.Fatalf("Deepgram got %d bytes, want only the 2 written after resume", got)
	}
	if got := waits.get(); len(got) != 0 {
		t.Fatalf("pause took the device-error backoff path, waits = %v", got)
	}
}

// The UI hears that the mic is paused, and that it is back on resume. The
// loop reports the pause after it closes the microphone, and only while
// capture is still paused, so the resume waits for that report: a resume
// right after the close left the loop nothing to report, and the test failed.
func TestPauseBroadcastsMicPausedThenResumed(t *testing.T) {
	hub := server.NewHub()
	events := hub.Subscribe()
	defer hub.Unsubscribe(events)
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()

	runMicLoop(t, capture, mic, io.Discard, func(time.Duration) {}, hub)
	mic.expectEvents(t, "stream")
	capture.Pause()
	mic.capture(t, []byte{1})
	mic.expectEvents(t, "close")
	statuses := []string{nextMicStatus(t, events)}
	capture.Resume()
	mic.expectEvents(t, "open", "stream")
	statuses = append(statuses, nextMicStatus(t, events))
	if want := []string{storage.ComponentStatusPaused, storage.ComponentStatusConnected}; !slices.Equal(statuses, want) {
		t.Fatalf("mic statuses = %v, want %v", statuses, want)
	}
}

// nextMicStatus is the status of the next microphone status event the hub
// sends.
func nextMicStatus(t *testing.T, events <-chan []byte) string {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case msg := <-events:
			var payload map[string]any
			if err := json.Unmarshal(msg, &payload); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if payload["type"] == "component_status" && payload["component"] == "mic" {
				return fmt.Sprint(payload["status"])
			}
		case <-deadline:
			t.Fatal("timed out waiting for a mic status event")
		}
	}
}

// A mic that comes up while capture is paused (the startup retry path) is
// closed before any audio is read.
func TestMicStartedWhilePausedIsClosedBeforeStreaming(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	mic := newScriptedMic()

	runMicLoop(t, capture, mic, io.Discard, func(time.Duration) {}, nil)
	mic.expectEvents(t, "close")

	capture.Resume()
	mic.expectEvents(t, "open", "stream")
}

// pauseBufferMic is a capture device that can hold audio captured during a
// pause (holdPauseAudio): Stream writes it first, and Close drops it, as
// closing a PortAudio stream drops its buffer. onOpen runs once, at the next
// Open or Reopen, after the device opened.
type pauseBufferMic struct {
	*scriptedMic
	onOpen   func()
	mu       sync.Mutex
	buffered []byte
}

func (m *pauseBufferMic) holdPauseAudio() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buffered = []byte("captured during the pause")
}

func (m *pauseBufferMic) opened() {
	if onOpen := m.onOpen; onOpen != nil {
		m.onOpen = nil
		onOpen()
	}
}

func (m *pauseBufferMic) Open() error {
	err := m.scriptedMic.Open()
	m.opened()
	return err
}

func (m *pauseBufferMic) Reopen() error {
	err := m.scriptedMic.Reopen()
	m.opened()
	return err
}

func (m *pauseBufferMic) Close() error {
	m.mu.Lock()
	m.buffered = nil
	m.mu.Unlock()
	return m.scriptedMic.Close()
}

func (m *pauseBufferMic) Stream(w io.Writer) error {
	m.mu.Lock()
	held := m.buffered
	m.buffered = nil
	m.mu.Unlock()
	if held != nil {
		if _, err := w.Write(held); err != nil {
			return err
		}
	}
	return m.scriptedMic.Stream(w)
}

// runPauseBufferMicLoop runs streamMicWithRetry on mic, with logf as its log,
// until the test ends.
func runPauseBufferMicLoop(t *testing.T, capture *captureGate, mic *pauseBufferMic, recorded io.Writer, logf func(string, ...any)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := goDone(func() {
		streamMicWithRetry(ctx, capture, mic, recorded, func(time.Duration) {}, logf, nil)
	})
	t.Cleanup(func() {
		cancel()
		close(mic.chunks)
		if !returnedWithin(done, testWait) {
			t.Error("mic loop did not stop")
		}
	})
}

// waitOpenIn waits up to testWait for the gate to open, from any goroutine:
// it reports a gate that stays closed with t.Error.
func waitOpenIn(t *testing.T, capture *captureGate) {
	opened, stop := context.WithTimeout(context.Background(), testWait)
	defer stop()
	if !capture.waitResumed(opened) {
		t.Error("the gate did not open again")
	}
}

// pauseAndResume pauses capture, resumes it and waits until the gate is open
// again, as a person who pauses and resumes at once does.
func pauseAndResume(t *testing.T, capture *captureGate) {
	capture.Pause()
	capture.Resume()
	waitOpenIn(t, capture)
}

// expectEventsInOrder fails the test unless the device's next calls include
// want in this order, within testWait; other calls between them are skipped.
func (m *scriptedMic) expectEventsInOrder(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.After(testWait)
	var seen []string
	for _, w := range want {
		for found := false; !found; {
			select {
			case got := <-m.events:
				seen = append(seen, got)
				found = got == w
			case <-deadline:
				t.Fatalf("device calls %v, want %v among them, in order", seen, want)
			}
		}
	}
}

// TestAPauseDuringTheMicOpenClosesTheDevice fails if the mic loop streams a
// device it opened before a pause that came during the open, also when a
// resume came during the open too and the gate is open again by the time
// Open returns: what the device captured during the pause would reach the
// recording. The loop must close that device, which drops what it holds,
// and open it again once capture resumes.
func TestAPauseDuringTheMicOpenClosesTheDevice(t *testing.T) {
	for _, resumeToo := range []bool{false, true} {
		name := "control, a pause alone during the open"
		if resumeToo {
			name = "a pause and a resume during the open"
		}
		t.Run(name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			capture.Pause()
			mic := &pauseBufferMic{scriptedMic: newScriptedMic()}
			recorded := &lockedBuffer{}
			runPauseBufferMicLoop(t, capture, mic, recorded, nopLogf)
			mic.expectEvents(t, "close")
			mic.onOpen = func() { // the device captures from here on
				if resumeToo {
					pauseAndResume(t, capture)
				} else {
					capture.Pause()
				}
				mic.holdPauseAudio()
			}
			capture.Resume()
			mic.expectEvents(t, "open", "close")
			if !resumeToo {
				capture.Resume()
			}
			mic.expectEvents(t, "open", "stream")
			mic.capture(t, []byte{1, 2})
			if got := recorded.Len(); got != 2 {
				t.Fatalf("the recording got %d bytes, want only the 2 captured after the resume", got)
			}
		})
	}
}

// TestNoAudioCapturedDuringAPauseReachesTheRecording fails if audio that a
// device captured during a pause reaches the recording, wherever the pause
// came while the device was open or opening, and however briefly it lasted.
// The gated writer drops audio from a microphone whose red confirmation began
// before a closing of the gate, and the mic loop then closes the device, which
// drops what it holds, and opens it again.
func TestNoAudioCapturedDuringAPauseReachesTheRecording(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, capture *captureGate, mic *pauseBufferMic, recorded io.Writer)
	}{
		{name: "a pause during the red confirmation, a resume during the open", run: func(t *testing.T, capture *captureGate, mic *pauseBufferMic, recorded io.Writer) {
			capture.Pause()
			runPauseBufferMicLoop(t, capture, mic, recorded, nopLogf)
			mic.expectEvents(t, "close")
			capture.beforeMicStart = sync.OnceValue(func() error {
				capture.Pause()
				return nil
			})
			mic.onOpen = func() { // the device opens during the pause
				capture.Resume()
				waitOpenIn(t, capture)
				mic.holdPauseAudio()
			}
			capture.Resume()
		}},
		{name: "a pause before the red confirmation began, a resume during the open", run: func(t *testing.T, capture *captureGate, mic *pauseBufferMic, recorded io.Writer) {
			capture.Pause()
			runPauseBufferMicLoop(t, capture, mic, recorded, nopLogf)
			mic.expectEvents(t, "close")
			var once sync.Once
			capture.beforeMicOpen = func(context.Context, <-chan struct{}) bool {
				once.Do(capture.Pause) // just after the hardware is ready
				return true
			}
			mic.onOpen = func() { // the device opens during the pause
				capture.Resume()
				waitOpenIn(t, capture)
				mic.holdPauseAudio()
			}
			capture.Resume()
		}},
		{name: "a pause and a resume during a reopen after a stream error", run: func(t *testing.T, capture *captureGate, mic *pauseBufferMic, recorded io.Writer) {
			mic.streamErrs = []error{errors.New("device disconnected")}
			mic.onOpen = func() {
				pauseAndResume(t, capture)
				mic.holdPauseAudio()
			}
			runPauseBufferMicLoop(t, capture, mic, recorded, nopLogf)
			mic.expectEventsInOrder(t, "reopen")
		}},
		{name: "a pause and a resume after the open's checks", run: func(t *testing.T, capture *captureGate, mic *pauseBufferMic, recorded io.Writer) {
			capture.Pause()
			var once sync.Once
			runPauseBufferMicLoop(t, capture, mic, recorded, func(format string, _ ...any) {
				if strings.HasPrefix(format, "microphone reopened") {
					once.Do(func() {
						pauseAndResume(t, capture)
						mic.holdPauseAudio()
					})
				}
			})
			mic.expectEvents(t, "close")
			capture.Resume()
		}},
		{name: "a pause and a resume before the loop's first stream", run: func(t *testing.T, capture *captureGate, mic *pauseBufferMic, recorded io.Writer) {
			// The start opened the device under a red confirmation; the
			// loop starts streaming it later.
			if err := capture.confirmMicOpen(); err != nil {
				t.Fatal(err)
			}
			capture.finishMicStart()
			pauseAndResume(t, capture)
			mic.holdPauseAudio()
			runPauseBufferMicLoop(t, capture, mic, recorded, nopLogf)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			mic := &pauseBufferMic{scriptedMic: newScriptedMic()}
			recorded := &lockedBuffer{}
			tc.run(t, capture, mic, recorded)
			mic.expectEventsInOrder(t, "open", "stream")
			mic.capture(t, []byte{1, 2})
			if got := recorded.Len(); got != 2 {
				t.Fatalf("the recording got %d bytes, want only the 2 captured after the resume", got)
			}
		})
	}
}

// Shutting down while paused ends the mic loop instead of leaving it waiting.
func TestPausedMicLoopStopsOnShutdown(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	mic := newScriptedMic()

	stop := runMicLoop(t, capture, mic, io.Discard, func(time.Duration) {}, nil)
	mic.expectEvents(t, "close")
	if !stop() {
		t.Fatal("mic loop did not exit on shutdown while paused")
	}
	mic.expectNoEvents(t)
}

// A device error that coincides with a pause is not reported as a mic failure:
// the loop closes the device at once, with no backoff.
func TestDeviceErrorDuringPauseClosesMicWithoutBackoff(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()
	mic.onStream = capture.Pause
	mic.streamErrs = []error{errors.New("device disconnected")}
	waits := &recordedWaits{}

	runMicLoop(t, capture, mic, io.Discard, waits.wait, nil)
	mic.expectEvents(t, "stream", "close")
	if got := waits.get(); len(got) != 0 {
		t.Fatalf("waits = %v, want none: a paused mic is closed at once", got)
	}
}

// pauseThenResume is a wait's extra work: a pause, and, with resumeFirst, a
// resume whose gate opens before the wait returns, as a busy machine can
// order them before the mic loop looks at the gate after its wait.
func pauseThenResume(capture *captureGate, resumeFirst bool) func() {
	return func() {
		capture.Pause()
		if !resumeFirst {
			return
		}
		capture.Resume()
		for deadline := time.Now().Add(testWait); capture.isClosed() && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	}
}

// openedUnderRed starts the mic loop on capture, paused, and resumes it, so
// the loop opens the microphone under a red confirmation, as the unit's own
// start-up does (startUnderRed): its first stream then fails with streamErr.
func openedUnderRed(t *testing.T, capture *captureGate, writer io.Writer, streamErr error, wait func(time.Duration)) *scriptedMic {
	t.Helper()
	capture.Pause()
	mic := newScriptedMic()
	mic.streamErrs = []error{streamErr}
	runMicLoop(t, capture, mic, writer, wait, nil)
	mic.expectEvents(t, "close")
	capture.Resume()
	mic.expectEvents(t, "open", "stream", "close")
	return mic
}

// A device error near a pause (USB drop, reconnect backoff) must not reopen
// the mic while capture is paused; resume brings it back, by its own open
// under its red confirmation, which ends its resuming state, also when the
// resume comes before the mic loop sees the pause.
func TestPausedMicIsNotReopenedAfterDeviceError(t *testing.T) {
	for _, resumeFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume before the loop looks %v", resumeFirst), func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			waits := &recordedWaits{then: pauseThenResume(capture, resumeFirst)} // the pause arrives during the reconnect backoff
			deepgram := &lockedBuffer{}
			mic := openedUnderRed(t, capture, deepgram, errors.New("device disconnected"), waits.wait)
			if !resumeFirst {
				// The loop closes the mic before its wait, so the close can
				// come before the pause the wait makes: resume only once the
				// pause has landed.
				waitForHardware(t, "the pause during the reconnect backoff", capture.isClosed)
				mic.expectNoEvents(t)
				capture.Resume()
			}
			mic.expectEvents(t, "open", "stream")
			waitMicHold(t, capture, "")
			mic.capture(t, []byte{1, 2, 3})
			if got := deepgram.Len(); got != 3 {
				t.Fatalf("Deepgram got %d bytes after resume, want 3", got)
			}
			if got := waits.get(); len(got) != 1 || got[0] != time.Second {
				t.Fatalf("backoff waits = %v, want [1s]", got)
			}
		})
	}
}

// The input-overflow restart must not reopen the mic either when a pause
// arrives during its short wait, nor once a resume comes before the mic loop
// sees the pause: that resume's own open, under its red confirmation, opens
// the microphone and ends its resuming state (CI run 44869249046 saw a
// reopen instead).
func TestPausedMicIsNotReopenedAfterOverflow(t *testing.T) {
	for _, resumeFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume before the loop looks %v", resumeFirst), func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			waits := &recordedWaits{then: pauseThenResume(capture, resumeFirst)}
			mic := openedUnderRed(t, capture, io.Discard, errors.New("Input overflowed"), waits.wait)
			if !resumeFirst {
				// The loop closes the mic before its wait, so the close can
				// come before the pause the wait makes: resume only once the
				// pause has landed.
				waitForHardware(t, "the pause during the overflow wait", capture.isClosed)
				capture.Resume()
			}
			mic.expectEvents(t, "open", "stream")
			waitMicHold(t, capture, "")
		})
	}
}

// TestARestartRaceOpensTheMicOnlyUnderRed fails if, when a pause and a
// resume both come during the mic loop's wait after an input overflow, the
// microphone opens, or audio reaches the gate, without the light confirmed
// red: a confirmation the light refuses must leave the microphone closed and
// no audio passing, however often the loop tries again. The converse, red
// confirmed, opens the microphone once, after the confirmation, and its audio
// passes. The red confirmation is the gate's own (beforeMicStart, the unit's
// recording light); each one is a "red" event among the device's calls.
func TestARestartRaceOpensTheMicOnlyUnderRed(t *testing.T) {
	for _, refused := range []bool{true, false} {
		t.Run(fmt.Sprintf("red refused %v", refused), func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			mic := newScriptedMic()
			mic.streamErrs = []error{errors.New("Input overflowed")}
			var refuse atomic.Bool
			var refusals atomic.Int64
			capture.beforeMicStart = func() error {
				if refuse.Load() {
					refusals.Add(1)
					return errors.New("the light did not read back red")
				}
				mic.events <- "red"
				return nil
			}
			var waits atomic.Int64
			wait := func(time.Duration) {
				if waits.Add(1) > 1 {
					time.Sleep(5 * time.Millisecond) // a later backoff
					return
				}
				// The overflow wait: the light stops confirming red, and a
				// pause and a resume both come before the loop looks.
				refuse.Store(refused)
				pauseThenResume(capture, true)()
			}
			gate := &lockedBuffer{}
			capture.Pause()
			runMicLoop(t, capture, mic, gate, wait, nil)
			mic.expectEvents(t, "close")
			capture.Resume()
			mic.expectEvents(t, "red", "open", "stream", "close") // opened under red; the overflow

			if refused {
				// The loop tries again and again; read every device call it
				// makes meanwhile.
				for window := time.After(300 * time.Millisecond); ; {
					select {
					case got := <-mic.events:
						if got != "close" {
							t.Fatalf("device call %q with the light refusing red, want none but closes", got)
						}
						continue
					case <-window:
					}
					break
				}
				if n := refusals.Load(); n < 3 {
					t.Fatalf("control: %d refused confirmations after the race, want the loop to have tried at least 3 times", n)
				}
				if n := gate.Len(); n != 0 {
					t.Fatalf("%d bytes reached the gate with the light refusing red, want none", n)
				}
				return
			}
			select {
			case got := <-mic.events:
				if got != "red" {
					t.Fatalf("device call %q before the red confirmation", got)
				}
			case <-time.After(testWait):
				t.Fatal("no red confirmation after the race")
			}
			select {
			case got := <-mic.events:
				if got != "open" && got != "reopen" {
					t.Fatalf("device call %q after the red confirmation, want the microphone's one open", got)
				}
			case <-time.After(testWait):
				t.Fatal("the microphone did not open after the red confirmation")
			}
			mic.expectEvents(t, "stream")
			mic.capture(t, []byte{1, 2, 3})
			if n := gate.Len(); n != 3 {
				t.Fatalf("%d bytes reached the gate, want the 3 captured under red", n)
			}
			mic.expectNoEvents(t)
		})
	}
}

// If the device cannot open on resume (unplugged during the pause), the mic
// loop holds no input device through the backoff, and falls back to the
// reconnect path, which re-initializes PortAudio.
func TestResumeFallsBackToReopenWhenMicCannotOpen(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	mic := newScriptedMic()
	mic.openErrs = []error{errors.New("microphone stream unavailable")}
	waits := &recordedWaits{}

	runMicLoop(t, capture, mic, io.Discard, waits.wait, nil)
	mic.expectEvents(t, "close")

	capture.Resume()
	mic.expectEvents(t, "open", "reopen", "stream")
	if got := waits.get(); len(got) != 1 || got[0] != time.Second {
		t.Fatalf("backoff waits = %v, want [1s]", got)
	}
}

// TestMicLoopEndsTheResumeWindowWithItsStart fails if the mic loop, opening
// the device again on resume, does not end the resume's resuming state once
// that start has finished, whether it opened the device or the light refused
// it: readiness and capture_state would say resuming until the stall limit.
func TestMicLoopEndsTheResumeWindowWithItsStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lightErr error
		want     []string
	}{
		{name: "started", want: []string{"open", "stream"}},
		{name: "light refused", lightErr: errors.New("light stayed off")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			capture.Pause()
			capture.beforeMicStart = func() error { return tc.lightErr }
			mic := newScriptedMic()
			runMicLoop(t, capture, mic, io.Discard, (&recordedWaits{}).wait, nil)
			mic.expectEvents(t, "close")

			capture.Resume()
			mic.expectEvents(t, tc.want...)
			waitMicHold(t, capture, "")
		})
	}
}

// waitMicHold fails the test unless capture's mic hold is want within testWait.
func waitMicHold(t *testing.T, capture *captureGate, want string) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	got := capture.MicHold()
	for got != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		got = capture.MicHold()
	}
	if got != want {
		t.Fatalf("mic hold = %q after %v, want %q", got, testWait, want)
	}
}

// waitOpen fails the test unless the gate opens within testWait.
func waitOpen(t *testing.T, capture *captureGate) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if !capture.waitResumed(ctx) {
		t.Fatal("gate did not open after Resume")
	}
}

// expectStaysClosed fails the test if the gate opens within a short wait.
func expectStaysClosed(t *testing.T, capture *captureGate, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if capture.waitResumed(ctx) {
		t.Fatalf("gate opened %s", when)
	}
}

// returnsAtOnce fails the test if f blocks: a control call must never wait for
// a session end.
func returnsAtOnce(t *testing.T, name string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatalf("%s blocked", name)
	}
}

// The gate's three steps run in order, once per transition: onPause before the
// gate closes (so no session can open behind a closed gate), settle after
// Pause returns, and beforeResume after settle and before the gate opens.
func TestCaptureGateRunsTransitionStepsInOrder(t *testing.T) {
	var capture *captureGate
	var mu sync.Mutex
	var steps []string
	step := func(name string) func() {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			steps = append(steps, fmt.Sprintf("%s(closed=%v)", name, capture.isClosed()))
		}
	}
	capture = newCaptureGate(step("onPause"), step("settle"), step("beforeResume"))

	capture.Pause()
	capture.Pause()
	mu.Lock()
	afterPause := slices.Clone(steps)
	mu.Unlock()
	if len(afterPause) == 0 || afterPause[0] != "onPause(closed=false)" {
		t.Fatalf("steps when Pause returned = %v, want onPause to have run before the gate closed", afterPause)
	}
	if !capture.isClosed() {
		t.Fatal("gate open after Pause returned")
	}

	capture.Resume()
	capture.Resume()
	if capture.IsPaused() {
		t.Fatal("switch still paused after Resume")
	}
	waitOpen(t, capture)
	want := []string{"onPause(closed=false)", "settle(closed=true)", "beforeResume(closed=true)"}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
}

// blockingWriter holds each Write open until the test releases it.
type blockingWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.entered <- struct{}{}
	<-w.release
	return len(p), nil
}

// A write that passed the gate and then hangs (a half-open Deepgram socket)
// holds the pause back only up to the write wait, so a Resume still gets the
// microphone back.
func TestCaptureGateSettlesPastAWedgedInflightWrite(t *testing.T) {
	settled := make(chan struct{})
	capture := newCaptureGate(nil, func() { close(settled) }, nil)
	capture.writeWait = 50 * time.Millisecond
	dst := &blockingWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(dst.release) })
	writer := capture.Writer(dst)
	go func() { _, _ = writer.Write([]byte{1}) }()
	<-dst.entered

	capture.Pause()
	select {
	case <-settled:
	case <-time.After(testWait):
		t.Fatal("pause did not settle while a write that passed the gate was hung")
	}
	capture.Resume()
	waitOpen(t, capture)
}

// Pause returns at once even while a write that passed the gate is still
// running (a slow Deepgram socket), and the pause settles only after that
// write is done, so the session ends after its last audio.
func TestCaptureGateSettlesAfterInflightWrite(t *testing.T) {
	settled := make(chan struct{})
	capture := newCaptureGate(nil, func() { close(settled) }, nil)
	dst := &blockingWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	writer := capture.Writer(dst)

	writeErr := make(chan error, 1)
	go func() {
		_, err := writer.Write([]byte{1})
		writeErr <- err
	}()
	<-dst.entered

	paused := make(chan struct{})
	go func() {
		capture.Pause()
		close(paused)
	}()
	select {
	case <-paused:
	case <-time.After(testWait):
		t.Fatal("Pause waited for an in-flight write")
	}
	if !capture.IsPaused() {
		t.Fatal("gate not paused after Pause returned")
	}
	select {
	case <-settled:
		t.Fatal("pause settled while a write was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(dst.release)
	if err := <-writeErr; err != nil {
		t.Fatalf("in-flight write failed: %v", err)
	}
	select {
	case <-settled:
	case <-time.After(testWait):
		t.Fatal("pause did not settle after the in-flight write finished")
	}
	if _, err := writer.Write([]byte{2}); !errors.Is(err, errGateClosed) {
		t.Fatalf("write after pause: got %v, want errGateClosed", err)
	}
}

// Resume and Pause return at once even while the pause is still settling (a
// long encode), so neither control blocks. The gate opens only once the pause
// has settled, and beforeResume runs once, after it.
func TestCaptureGateControlsDoNotWaitForSettle(t *testing.T) {
	release := make(chan struct{})
	var settled, resumedEarly atomic.Bool
	var beforeResumes atomic.Int32
	capture := newCaptureGate(nil,
		func() {
			<-release
			settled.Store(true)
		},
		func() {
			if !settled.Load() {
				resumedEarly.Store(true)
			}
			beforeResumes.Add(1)
		},
	)

	capture.Pause()
	returnsAtOnce(t, "Resume", capture.Resume)
	if capture.IsPaused() {
		t.Fatal("switch still paused after Resume")
	}
	if !capture.isClosed() {
		t.Fatal("gate opened before the pause settled")
	}
	returnsAtOnce(t, "Pause", capture.Pause)
	returnsAtOnce(t, "Resume", capture.Resume)

	close(release)
	waitOpen(t, capture)
	if resumedEarly.Load() {
		t.Fatal("beforeResume ran before the pause settled")
	}
	if got := beforeResumes.Load(); got != 1 {
		t.Fatalf("beforeResume ran %d times, want 1", got)
	}
}

func TestCaptureGateStaysClosedWhenBeforeResumeRepause(t *testing.T) {
	var capture *captureGate
	capture = newCaptureGate(nil, nil, func() {
		capture.mu.Lock()
		capture.switchOff = true
		capture.mu.Unlock()
	})
	capture.Pause()
	capture.Resume()
	expectStaysClosed(t, capture, "resume was rejected")
	if !capture.IsPaused() {
		t.Fatal("rejected resume cleared the paused switch")
	}
}

// A Pause before the pause settled cancels a pending Resume.
func TestCaptureGatePauseCancelsPendingResume(t *testing.T) {
	release := make(chan struct{})
	capture := newCaptureGate(nil, func() { <-release }, nil)

	capture.Pause()
	returnsAtOnce(t, "Resume", capture.Resume)
	returnsAtOnce(t, "Pause", capture.Pause)
	close(release)
	expectStaysClosed(t, capture, "after a Pause cancelled the Resume")
	if !capture.IsPaused() {
		t.Fatal("switch not paused after the last Pause")
	}

	capture.Resume()
	waitOpen(t, capture)
}

// A Resume left waiting from an earlier pause must not open the gate during a
// later pause that has not settled yet. The first pause settles only once both
// Resumes wait on it: had the first Resume opened the gate before the second
// Pause, that Pause would have been the later pause, whose settle the test
// holds, and the gate could not open (`gate did not open after Resume`, seen
// in go test -race ./... for #79).
func TestCaptureGateStaleResumeDoesNotOpenALaterPause(t *testing.T) {
	first, release := make(chan struct{}), make(chan struct{})
	var settles atomic.Int32
	capture := newCaptureGate(nil, func() {
		switch settles.Add(1) {
		case 1:
			<-first
		case 2:
			<-release
		}
	}, nil)

	// Two Resumes wait on the first pause; one of them opens the gate.
	capture.Pause()
	capture.Resume()
	capture.Pause()
	capture.Resume()
	close(first)
	waitOpen(t, capture)

	capture.Pause()
	capture.Resume()
	expectStaysClosed(t, capture, "before the second pause settled")
	close(release)
	waitOpen(t, capture)
}

// TestCaptureGateStaleOpeningDoesNotOpenALaterPause fails if a gate opening
// left from an earlier pause opens the gate during a later pause that has not
// settled yet. openAfter checks the light's stored pattern before it takes
// control, so an opening can sit in that check while a pause and a resume go
// by. Two resumes wait on the first pause; the opening that wins waits until
// the other is in its check too, and opens the gate. The stale one is held in
// its check until a second pause has closed the gate and a resume waits on
// that pause's settle; released then, it must find the later closing and do
// nothing (red team, review 5969364339 of #79). It holds the stale opening on
// purpose, where TestCaptureGateStaleResumeDoesNotOpenALaterPause needs the
// second Pause to win control from it.
func TestCaptureGateStaleOpeningDoesNotOpenALaterPause(t *testing.T) {
	first, release := make(chan struct{}), make(chan struct{})
	var settles atomic.Int32
	capture := newCaptureGate(nil, func() {
		switch settles.Add(1) {
		case 1:
			<-first
		case 2:
			<-release
		}
	}, nil)
	bothChecking, staleRelease := make(chan struct{}), make(chan struct{})
	var staleOnce, releaseOnce sync.Once
	letStale := func() { staleOnce.Do(func() { close(staleRelease) }) }
	letSecond := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { letStale(); letSecond() })
	var checks atomic.Int32
	capture.beforeGateOpen = func() error { return nil }
	capture.checkLightPattern = func() error {
		switch checks.Add(1) {
		case 1: // the opening that wins waits until the other is in its check
			select {
			case <-bothChecking:
			case <-time.After(testWait):
			}
		case 2: // the stale opening, held until a later pause closed the gate
			close(bothChecking)
			select {
			case <-staleRelease:
			case <-time.After(testWait):
			}
		}
		return nil
	}

	capture.Pause()
	capture.Resume()
	capture.Pause()
	capture.Resume()
	close(first)
	select {
	case <-bothChecking:
	case <-time.After(testWait):
		t.Fatal("control: the two gate openings did not both reach their light check")
	}
	waitOpen(t, capture)

	capture.Pause() // the later pause; its settle is held
	capture.Resume()
	letStale()
	time.Sleep(50 * time.Millisecond) // the stale opening takes control and looks
	expectStaysClosed(t, capture, "from a gate opening left from an earlier pause, before the later pause settled")
	letSecond()
	waitOpen(t, capture)
}

// A Resume that comes after a Pause still waiting for the gate (the gate busy
// reopening) is not lost: the last call wins and the gate opens.
func TestCaptureGateResumeAfterAQueuedPauseIsNotLost(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	capture := newCaptureGate(nil, nil, func() {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	})

	capture.Pause()
	capture.Resume()
	<-entered // the gate is reopening and holds its transition lock

	go capture.Pause()
	time.Sleep(50 * time.Millisecond) // let that Pause queue for the gate
	resumed := make(chan struct{})
	go func() {
		capture.Resume()
		close(resumed)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-resumed

	waitOpen(t, capture)
	if capture.IsPaused() {
		t.Fatal("switch paused after the last call, a Resume")
	}
}

// Pause and Resume from many callers at once (HTTP requests, a hardware
// button) neither panic nor leave the gate half switched.
func TestCaptureGateConcurrentPauseResume(t *testing.T) {
	capture := newCaptureGate(func() {}, func() {}, func() {})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				if (i+j)%2 == 0 {
					capture.Pause()
				} else {
					capture.Resume()
				}
			}
		}()
	}
	wg.Wait()

	capture.Resume()
	if capture.IsPaused() {
		t.Fatal("switch paused after a final Resume")
	}
	waitOpen(t, capture)
}

// The mic loop's reconnect backoff (up to 30 s) ends when the gate closes, so
// a pause closes the device at once instead of after the backoff.
func TestSleepUnlessPausedEndsWhenTheGateCloses(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	slept := make(chan struct{})
	go func() {
		capture.sleepUnlessPaused(context.Background(), time.Hour)
		close(slept)
	}()
	select {
	case <-slept:
		t.Fatal("sleep ended with the gate open")
	case <-time.After(50 * time.Millisecond):
	}

	capture.Pause()
	select {
	case <-slept:
	case <-time.After(testWait):
		t.Fatal("sleep did not end when the gate closed")
	}
	returnsAtOnce(t, "a sleep with the gate already closed", func() { capture.sleepUnlessPaused(context.Background(), time.Hour) })
}

// The mic loop's backoff and overflow waits end at shutdown, so the loop ends
// before shutdown stops the device and terminates PortAudio.
func TestSleepUnlessPausedEndsAtShutdown(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	slept := goDone(func() { capture.sleepUnlessPaused(ctx, time.Hour) })
	if returnedWithin(slept, 50*time.Millisecond) {
		t.Fatal("sleep ended with the gate open and no shutdown")
	}
	cancel()
	if !returnedWithin(slept, testWait) {
		t.Fatal("sleep did not end at shutdown")
	}
}

// At shutdown a streaming mic loop ends at the device's next buffer, which
// goes nowhere. Shutdown waits for the loop before it stops the device and
// terminates PortAudio: doing either under a read in progress crashes inside
// PortAudio.
func TestMicLoopEndsAtShutdownWhileStreaming(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()
	recorded := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	ended := goDone(func() {
		streamMicWithRetry(ctx, capture, mic, recorded, func(time.Duration) {}, func(string, ...any) {}, nil)
	})
	t.Cleanup(func() {
		cancel()
		close(mic.chunks)
		<-ended
	})
	mic.expectEvents(t, "stream")
	mic.capture(t, make([]byte, 100))

	cancel()
	mic.capture(t, make([]byte, 100))
	if !returnedWithin(ended, testWait) {
		t.Fatal("mic loop still streaming after shutdown")
	}
	if got := recorded.Len(); got != 100 {
		t.Fatalf("recorded %d bytes, want only the 100 captured before shutdown", got)
	}
}

// joinMicLoops waits for the mic loops, but not for good: a loop stuck in a
// read (a device that stopped delivering audio) is reported, so shutdown
// leaves the device and PortAudio to the process exit.
func TestJoinMicLoopsReportsALoopStillRunning(t *testing.T) {
	var loops sync.WaitGroup
	release := make(chan struct{})
	loops.Go(func() { <-release })
	if joinMicLoops(&loops, 50*time.Millisecond) {
		t.Fatal("joined a mic loop that is still running")
	}
	close(release)
	if !joinMicLoops(&loops, testWait) {
		t.Fatal("mic loops not joined after they ended")
	}
}

type fakeMicState bool

func (f fakeMicState) IsOpen() bool { return bool(f) }

func TestCaptureGateCapturingNeedsOpenGateAndOpenMic(t *testing.T) {
	for _, tc := range []struct {
		paused, micOpen, want bool
	}{
		{paused: false, micOpen: true, want: true},
		{paused: false, micOpen: false, want: false},
		{paused: true, micOpen: true, want: false},
		{paused: true, micOpen: false, want: false},
	} {
		capture := newCaptureGate(nil, nil, nil)
		if tc.paused {
			capture.Pause()
		}
		if got := capture.capturing(fakeMicState(tc.micOpen)); got != tc.want {
			t.Errorf("capturing(paused=%v, micOpen=%v) = %v, want %v", tc.paused, tc.micOpen, got, tc.want)
		}
	}
}

// callLog records calls from the fakes below, in order.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func newCallLog() *callLog { return &callLog{} }

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// waitFor polls until call has been logged; the pause settles in the background.
func (l *callLog) waitFor(t *testing.T, call string) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !slices.Contains(l.get(), call) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q; calls so far: %v", call, l.get())
		}
		time.Sleep(time.Millisecond)
	}
}

// fakeSessions stands in for *session.Manager.
type fakeSessions struct {
	log       *callLog
	mu        sync.Mutex
	active    string
	finalized chan struct{}
	endErr    error // when set, every ForceEndSession fails with it
}

func (f *fakeSessions) PauseCapture()         { f.log.add("pause-capture") }
func (f *fakeSessions) WaitForSessionStarts() { f.log.add("wait-starts") }
func (f *fakeSessions) ResumeCapture()        { f.log.add("resume-capture") }

func (f *fakeSessions) ActiveSession() (string, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active, time.Time{}
}

func (f *fakeSessions) AwaitFinalize() <-chan struct{} {
	f.log.add("await-finalize")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalized = make(chan struct{})
	return f.finalized
}

// finalizeReply delivers Deepgram's reply to Finalize.
func (f *fakeSessions) finalizeReply() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.finalized)
}

func (f *fakeSessions) ForceEndSession(context.Context) error {
	f.log.add("end-session")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.endErr != nil {
		return f.endErr
	}
	if f.active == "" {
		return session.ErrNoActiveSession
	}
	f.active = ""
	return nil
}

type fakeDeepgram struct {
	log     *callLog
	onReply func()
}

func (f *fakeDeepgram) Finalize() error {
	f.log.add("finalize")
	if f.onReply != nil {
		f.onReply()
	}
	return nil
}

type fakePreroll struct{ log *callLog }

func (f fakePreroll) DiscardPreroll() { f.log.add("discard-preroll") }

func nopLogf(string, ...any) {}

// Pausing blocks new sessions at once. The pause then waits out a session
// start already under way, and the session that was recording ends only after
// Deepgram's reply to Finalize is in, so its last words are kept.
func TestPauseEndsSessionAfterDeepgramsLastWords(t *testing.T) {
	log := newCallLog()
	sessions := &fakeSessions{log: log, active: "s1"}
	dg := &fakeDeepgram{log: log, onReply: sessions.finalizeReply}
	capture := newSessionCaptureGate(context.Background(), sessions, fakePreroll{log}, func() deepgramFinalizer { return dg }, time.Hour, testEndRetry, nopLogf)

	capture.Pause()
	if got := log.get(); len(got) == 0 || got[0] != "pause-capture" {
		t.Fatalf("calls when Pause returned = %v, want new sessions blocked first", got)
	}
	log.waitFor(t, "end-session")
	if got, want := log.get(), []string{"pause-capture", "wait-starts", "await-finalize", "finalize", "end-session"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// If Deepgram never replies (nothing was buffered, or the socket is down), the
// session still ends once the finalize wait runs out.
func TestPauseEndsSessionWhenDeepgramDoesNotReply(t *testing.T) {
	log := newCallLog()
	sessions := &fakeSessions{log: log, active: "s1"}
	dg := &fakeDeepgram{log: log}
	capture := newSessionCaptureGate(context.Background(), sessions, fakePreroll{log}, func() deepgramFinalizer { return dg }, 20*time.Millisecond, testEndRetry, nopLogf)

	capture.Pause()
	log.waitFor(t, "end-session")
	log.waitFor(t, "finalize")
}

// Without a Deepgram client the session ends without waiting.
func TestPauseWithoutDeepgramEndsSessionAtOnce(t *testing.T) {
	log := newCallLog()
	sessions := &fakeSessions{log: log, active: "s1"}
	capture := newSessionCaptureGate(context.Background(), sessions, fakePreroll{log}, func() deepgramFinalizer { return nil }, time.Hour, testEndRetry, nopLogf)

	capture.Pause()
	log.waitFor(t, "end-session")
	if got, want := log.get(), []string{"pause-capture", "wait-starts", "end-session"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// With no session recording there is nothing to wait for, but Deepgram is
// still told to finalize, so audio it holds does not reach a later session.
func TestPauseWithNoSessionStillFinalizesDeepgram(t *testing.T) {
	log := newCallLog()
	sessions := &fakeSessions{log: log}
	dg := &fakeDeepgram{log: log}
	capture := newSessionCaptureGate(context.Background(), sessions, fakePreroll{log}, func() deepgramFinalizer { return dg }, time.Hour, testEndRetry, nopLogf)

	capture.Pause()
	log.waitFor(t, "end-session")
	log.waitFor(t, "finalize")
	if slices.Contains(log.get(), "await-finalize") {
		t.Fatalf("calls = %v: waited for a reply with no session to save it to", log.get())
	}
}

// Resume drops preroll audio from before the pause and lets sessions start
// again, both before audio flows.
func TestResumeDropsPrerollAndAllowsSessionsBeforeAudioFlows(t *testing.T) {
	log := newCallLog()
	sessions := &fakeSessions{log: log}
	var capture *captureGate
	var closedAtDiscard atomic.Bool
	preroll := prerollFunc(func() {
		closedAtDiscard.Store(capture.isClosed())
		log.add("discard-preroll")
	})
	capture = newSessionCaptureGate(context.Background(), sessions, preroll, func() deepgramFinalizer { return nil }, time.Hour, testEndRetry, nopLogf)

	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	if got, want := log.get(), []string{"pause-capture", "wait-starts", "end-session", "discard-preroll", "resume-capture"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if !closedAtDiscard.Load() {
		t.Fatal("preroll was dropped after audio started flowing again")
	}
}

// The gate stays closed while Deepgram's Finalize call is still running (the
// SDK can block while it redials), within the finalize wait.
func TestResumeWaitsForDeepgramFinalizeCall(t *testing.T) {
	log := newCallLog()
	sessions := &fakeSessions{log: log}
	release := make(chan struct{})
	dg := &fakeDeepgram{log: log, onReply: func() { <-release }}
	capture := newSessionCaptureGate(context.Background(), sessions, fakePreroll{log}, func() deepgramFinalizer { return dg }, time.Hour, testEndRetry, nopLogf)

	capture.Pause()
	returnsAtOnce(t, "Resume", capture.Resume)
	log.waitFor(t, "finalize")
	expectStaysClosed(t, capture, "while Deepgram's Finalize call was still running")
	close(release)
	waitOpen(t, capture)
}

// A Finalize call that never returns (a half-open Deepgram socket) must not
// keep the microphone off: past the finalize wait the session ends anyway and
// a Resume opens the gate.
func TestWedgedDeepgramFinalizeDoesNotKeepTheMicOff(t *testing.T) {
	log := newCallLog()
	sessions := &fakeSessions{log: log, active: "s1"}
	wedged := make(chan struct{})
	t.Cleanup(func() { close(wedged) })
	dg := &fakeDeepgram{log: log, onReply: func() { <-wedged }}
	capture := newSessionCaptureGate(context.Background(), sessions, fakePreroll{log}, func() deepgramFinalizer { return dg }, 20*time.Millisecond, testEndRetry, nopLogf)

	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	if got := log.get(); !slices.Contains(got, "end-session") {
		t.Fatalf("calls = %v, want the session ended before the gate opened", got)
	}
}

// The switch on with no audio flowing is reported as it is: resuming at
// first, then stalled once that lasts past the stall limit, never paused.
func TestCaptureStateReportsAStalledResume(t *testing.T) {
	release := make(chan struct{})
	capture := newCaptureGate(nil, func() { <-release }, nil)
	capture.stallAfter = 50 * time.Millisecond
	open := fakeMicState(true)

	if got := capture.captureState(open); got != captureCapturing {
		t.Fatalf("state before any pause = %q, want %q", got, captureCapturing)
	}
	capture.Pause()
	if got := capture.captureState(open); got != capturePaused {
		t.Fatalf("state while paused = %q, want %q", got, capturePaused)
	}
	if got := capture.MicHold(); got != capturePaused {
		t.Fatalf("mic hold while paused = %q, want %q", got, capturePaused)
	}

	capture.Resume()
	if got := capture.captureState(open); got != captureResuming {
		t.Fatalf("state right after Resume = %q, want %q", got, captureResuming)
	}
	time.Sleep(100 * time.Millisecond)
	if got := capture.captureState(open); got != captureStalled {
		t.Fatalf("state past the stall limit = %q, want %q", got, captureStalled)
	}
	if got := capture.MicHold(); got != captureStalled {
		t.Fatalf("mic hold past the stall limit = %q, want %q", got, captureStalled)
	}

	close(release)
	waitOpen(t, capture)
	if got := capture.captureState(open); got != captureCapturing {
		t.Fatalf("state after the gate opened = %q, want %q", got, captureCapturing)
	}
	if got := capture.captureState(fakeMicState(false)); got != captureMicUnavailable {
		t.Fatalf("state with the gate open and the mic closed = %q, want %q", got, captureMicUnavailable)
	}
	if got := capture.MicHold(); got != "" {
		t.Fatalf("mic hold with the gate open = %q, want none", got)
	}
}

// TestResumingLastsUntilTheResumeStartFinishes fails if the capture stops
// reporting resuming as soon as the gate opens, before the start that brings
// the microphone and Deepgram back has finished: readiness then answers 503
// for a unit that is only resuming, and capture_state says the microphone is
// unavailable. The start finishing ends it, a start finishing for an earlier
// resume does not end a later one, and a pause reports paused.
func TestResumingLastsUntilTheResumeStartFinishes(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	closedMic := fakeMicState(false)
	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	if got := capture.MicHold(); got != captureResuming {
		t.Fatalf("mic hold with the gate open and the resume's start not finished = %q, want %q", got, captureResuming)
	}
	if got := capture.captureState(closedMic); got != captureResuming {
		t.Fatalf("capture state with the gate open and the resume's start not finished = %q, want %q", got, captureResuming)
	}

	capture.finishResume()
	if got := capture.MicHold(); got != "" {
		t.Fatalf("mic hold once the resume's start finished = %q, want none", got)
	}
	if got := capture.captureState(closedMic); got != captureMicUnavailable {
		t.Fatalf("capture state once the resume's start finished with the mic closed = %q, want %q", got, captureMicUnavailable)
	}

	capture.Pause()
	capture.finishResume() // a start for the earlier resume, finishing during the pause
	if got := capture.MicHold(); got != capturePaused {
		t.Fatalf("mic hold while paused = %q, want %q", got, capturePaused)
	}
	capture.Resume()
	waitOpen(t, capture)
	if got := capture.MicHold(); got != captureResuming {
		t.Fatalf("mic hold after a later resume, its own start not finished = %q, want %q", got, captureResuming)
	}
}

// TestResumingEndsAtTheStallLimit fails if a resume whose start never
// finishes keeps reporting resuming past the stall limit, which would hide a
// microphone or Deepgram that does not come back.
func TestResumingEndsAtTheStallLimit(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.stallAfter = 50 * time.Millisecond
	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	time.Sleep(2 * capture.stallAfter)
	if got := capture.MicHold(); got != "" {
		t.Fatalf("mic hold with the gate open past the stall limit = %q, want none", got)
	}
	if got := capture.captureState(fakeMicState(false)); got != captureMicUnavailable {
		t.Fatalf("capture state past the stall limit with the mic closed = %q, want %q", got, captureMicUnavailable)
	}
}

// storeSessions returns a session manager on a real SQLite store, which has
// one database connection. At the test's end the manager's background work,
// the summaries of the sessions it ended, stops before the store closes, as
// main's stop does.
func storeSessions(t *testing.T) (*storage.SQLiteStore, *session.Manager) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.StopBackground(ctx); err != nil {
			t.Errorf("the session manager's background work did not stop: %v", err)
		}
	})
	return store, manager
}

// holdDatabase takes the store's one connection, as a long search query or an
// import transaction does, until release is called or the test ends.
func holdDatabase(t *testing.T, store *storage.SQLiteStore) (release func()) {
	t.Helper()
	conn, err := store.DB().Conn(context.Background())
	if err != nil {
		t.Fatalf("take the database connection: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = conn.Close() }) }
	t.Cleanup(release)
	return release
}

// startSessionInFlight starts a session, as the Record button does, and
// returns once the start is under way. With the database held, the start
// waits on it.
func startSessionInFlight(t *testing.T, manager *session.Manager) (id string, started <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := manager.ManualStartSession(context.Background(), "")
		done <- err
	}()
	deadline := time.Now().Add(testWait)
	for {
		if id, _ := manager.ActiveSession(); id != "" {
			return id, done
		}
		if time.Now().After(deadline) {
			t.Fatal("the session start did not begin")
		}
		time.Sleep(time.Millisecond)
	}
}

// goDone runs f in the background and returns a channel closed when f returns.
func goDone(f func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	return done
}

// returnedWithin reports whether done is closed within d.
func returnedWithin(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// expectOnlySessionEnded fails the test unless session id has ended and is the
// only session in the store: none opened during the pause.
func expectOnlySessionEnded(t *testing.T, store *storage.SQLiteStore, manager *session.Manager, id string) {
	t.Helper()
	if active, _ := manager.ActiveSession(); active != "" {
		t.Fatalf("session %s still active after the pause settled", active)
	}
	got, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session %s: %v", id, err)
	}
	if got.Status != "ended" {
		t.Fatalf("session %s is %q after the pause settled, want ended", id, got.Status)
	}
	var sessions int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("store has %d sessions, want 1", sessions)
	}
}

// Pause shuts the gate at once, even while a session start waits on the
// database (Record pressed as the user pauses, with the one SQLite connection
// busy): it does not wait for the start, and no audio passes the gate after
// Pause is called. The pause then ends that session once it has started, and
// no other session opens.
func TestPauseShutsTheGateWhileASessionStartWaits(t *testing.T) {
	store, manager := storeSessions(t)
	capture := newSessionCaptureGate(context.Background(), manager, fakePreroll{newCallLog()}, func() deepgramFinalizer { return nil }, time.Hour, testEndRetry, nopLogf)
	recorded := &lockedBuffer{}
	mic := capture.Writer(recorded)

	releaseDB := holdDatabase(t, store)
	id, started := startSessionInFlight(t, manager)

	paused := goDone(capture.Pause)
	if !returnedWithin(paused, time.Second) {
		t.Error("Pause still waiting for the session start after 1s")
	}
	if n, err := mic.Write(make([]byte, 3200)); n != 0 || !errors.Is(err, errGateClosed) || recorded.Len() != 0 {
		t.Errorf("audio written after Pause was called: %d bytes passed the gate (write returned %d, %v), want 0", recorded.Len(), n, err)
	}

	releaseDB()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("session start failed: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("session start did not finish after the database was freed")
	}
	if !returnedWithin(paused, testWait) {
		t.Fatal("Pause did not return after the database was freed")
	}
	capture.Resume()
	waitOpen(t, capture)
	expectOnlySessionEnded(t, store, manager, id)
}

// A database that stays busy (SQLite has one connection) keeps a resumed
// capture's gate closed: the pause cannot end the session that was recording,
// or let a session start already under way finish, until the database
// answers, and opening the gate first would file speech after the resume into
// that session. Pause and Resume still return at once, the capture reports
// stalled, not paused or capturing, and the gate opens once the database is
// free.
func TestStuckDatabaseKeepsTheGateClosedAndReportsStalled(t *testing.T) {
	for _, tc := range []struct {
		name          string
		startInFlight bool // the database is busy from before the session start, not only at its end
	}{
		{name: "session end waits", startInFlight: false},
		{name: "session start waits", startInFlight: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, manager := storeSessions(t)
			capture := newSessionCaptureGate(context.Background(), manager, fakePreroll{newCallLog()}, func() deepgramFinalizer { return nil }, time.Hour, testEndRetry, nopLogf)
			capture.stallAfter = 50 * time.Millisecond

			var id string
			var releaseDB func()
			if tc.startInFlight {
				releaseDB = holdDatabase(t, store)
				id, _ = startSessionInFlight(t, manager)
			} else {
				var err error
				if id, err = manager.ManualStartSession(context.Background(), ""); err != nil {
					t.Fatalf("start session: %v", err)
				}
				releaseDB = holdDatabase(t, store)
			}

			paused := goDone(capture.Pause)
			for deadline := time.Now().Add(testWait); !capture.IsPaused(); time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("Pause did not move the switch off")
				}
			}
			resumed := goDone(capture.Resume)
			if !returnedWithin(paused, time.Second) {
				t.Error("Pause blocked by the stuck database")
			}
			if !returnedWithin(resumed, time.Second) {
				t.Error("Resume blocked by the stuck database")
			}
			time.Sleep(2 * capture.stallAfter)
			if got := capture.captureState(fakeMicState(true)); got != captureStalled {
				t.Errorf("capture state with the database stuck = %q, want %q", got, captureStalled)
			}
			if capture.capturing(fakeMicState(true)) {
				t.Error("gate open with the database stuck, before the session from before the pause ended")
			}

			releaseDB()
			if !returnedWithin(paused, testWait) || !returnedWithin(resumed, testWait) {
				t.Fatal("Pause or Resume did not return after the database was freed")
			}
			waitOpen(t, capture)
			expectOnlySessionEnded(t, store, manager, id)
		})
	}
}

// refusingStore is a SQLite store whose session ends fail while refuse is set,
// as on a database that errors (a full disk, an I/O error).
type refusingStore struct {
	*storage.SQLiteStore
	refuse atomic.Bool
}

func (s *refusingStore) EndSession(id string, endedAt time.Time, audioPath string) error {
	if s.refuse.Load() {
		return errors.New("disk I/O error")
	}
	return s.SQLiteStore.EndSession(id, endedAt, audioPath)
}

// blockRecordingFiles puts a directory where each file the recording of
// session id is saved to would go, so every encoder and the WAV fallback fail
// to save it, as on a full SD card. unblock removes them.
func blockRecordingFiles(t *testing.T, audioDir, id string) (unblock func()) {
	t.Helper()
	var paths []string
	for _, ext := range []string{".mp3", ".wav"} {
		path := filepath.Join(audioDir, id+ext)
		// Not empty, so the recorder cannot remove it as a partial file.
		if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
			t.Fatalf("block %s: %v", path, err)
		}
		paths = append(paths, path)
	}
	return func() {
		for _, path := range paths {
			if err := os.RemoveAll(path); err != nil {
				t.Errorf("unblock %s: %v", path, err)
			}
		}
	}
}

// speechFinal is Deepgram's final result for text, at the end of speech.
func speechFinal(t *testing.T, text string) *api.MessageResponse {
	t.Helper()
	raw := fmt.Sprintf(`{"is_final": true, "speech_final": true, "channel": {"alternatives": [{
		"transcript": %q, "words": [{"speaker": 0, "punctuated_word": %q, "start": 0, "end": 1}]}]}}`, text, text)
	var msg api.MessageResponse
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("build message: %v", err)
	}
	return &msg
}

// A pause whose session end fails (the database refuses the write, or the
// recording cannot be saved) keeps the gate closed and retries the end: opening
// the gate with the session from before the pause still current would file
// speech after the resume into it. The capture reports stalled meanwhile.
// Once the end succeeds, the session is ended with its recording, the gate
// opens, and speech after the resume starts a new session.
func TestFailedSessionEndKeepsTheGateClosedUntilItSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(t *testing.T, store *refusingStore, audioDir, id string) (restore func())
	}{
		{name: "the database refuses the end", fail: func(_ *testing.T, store *refusingStore, _, _ string) func() {
			store.refuse.Store(true)
			return func() { store.refuse.Store(false) }
		}},
		{name: "the recorder cannot save the recording", fail: func(t *testing.T, _ *refusingStore, audioDir, id string) func() {
			return blockRecordingFiles(t, audioDir, id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store := &refusingStore{SQLiteStore: db}
			audioDir := t.TempDir()
			recorder := audio.NewRecorder(audioDir)
			manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
			// Retries 50 ms apart, then 100, 200 and 400 ms: the failure clears
			// (about 100 ms in) well before the fifth attempt, after which the
			// recorder's failures give the recording up.
			capture := newSessionCaptureGate(context.Background(), manager, recorder, func() deepgramFinalizer { return nil }, time.Hour, 50*time.Millisecond, nopLogf)
			capture.stallAfter = 50 * time.Millisecond
			mic := capture.Writer(recorder.Writer(io.Discard))

			before, err := manager.ManualStartSession(context.Background(), "")
			if err != nil {
				t.Fatalf("start session: %v", err)
			}
			if _, err := mic.Write(make([]byte, 3200)); err != nil {
				t.Fatalf("record audio: %v", err)
			}
			restore := tc.fail(t, store, audioDir, before)

			capture.Pause()
			capture.Resume()
			time.Sleep(2 * capture.stallAfter)
			if capture.capturing(fakeMicState(true)) {
				t.Errorf("gate open while the session from before the pause could not be ended")
			} else if got := capture.captureState(fakeMicState(true)); got != captureStalled {
				t.Errorf("capture state while the session end fails = %q, want %q", got, captureStalled)
			}

			restore()
			waitOpen(t, capture)
			if err := manager.Message(speechFinal(t, "said after the resume")); err != nil {
				t.Fatalf("Message after the resume: %v", err)
			}
			if after, _ := manager.ActiveSession(); after == "" || after == before {
				t.Errorf("speech from after the resume went to session %q, want a new session, not the pre-pause session %s", after, before)
			}
			got, err := db.GetSession(before)
			if err != nil {
				t.Fatalf("read session %s: %v", before, err)
			}
			if got.Status != "ended" {
				t.Errorf("pre-pause session %s is %q, want ended", before, got.Status)
			}
			if got.AudioPath == "" {
				t.Errorf("pre-pause session %s has no recording", before)
			} else if _, err := os.Stat(got.AudioPath); err != nil {
				t.Errorf("pre-pause session %s recording: %v", before, err)
			}
		})
	}
}

// A recording that can never be saved does not keep the microphone off for
// good. After the recorder has failed to save it five times, the pause ends
// the session without it, leaving the raw audio on disk for a later save,
// and the gate opens onto no session: speech after the resume starts a new
// one, never the session from before the pause.
func TestPauseEndsASessionWhoseRecordingCannotBeSaved(t *testing.T) {
	db, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	audioDir := t.TempDir()
	recorder := audio.NewRecorder(audioDir)
	manager := session.NewManager(db, recorder, nil, nil, session.NewDetector(time.Hour), 0)
	capture := newSessionCaptureGate(context.Background(), manager, recorder, func() deepgramFinalizer { return nil }, time.Hour, testEndRetry, nopLogf)
	mic := capture.Writer(recorder.Writer(io.Discard))

	before, err := manager.ManualStartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if _, err := mic.Write(make([]byte, 3200)); err != nil {
		t.Fatalf("record audio: %v", err)
	}
	blockRecordingFiles(t, audioDir, before) // for good

	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)

	got, err := db.GetSession(before)
	if err != nil {
		t.Fatalf("read session %s: %v", before, err)
	}
	if got.Status != "ended" || got.AudioPath != "" {
		t.Errorf("pre-pause session %s is %q with recording %q, want ended with none", before, got.Status, got.AudioPath)
	}
	if got.RefinementStatus != storage.RefinementNoAudio {
		t.Errorf("pre-pause session %s refinement_status %q, want %q: it can never be refined", before, got.RefinementStatus, storage.RefinementNoAudio)
	}
	if size := fileSize(t, filepath.Join(audioDir, before+".pcm")); size != 3200 {
		t.Errorf("raw recording kept for recovery has %d bytes, want the 3200 recorded", size)
	}
	if err := manager.Message(speechFinal(t, "said after the resume")); err != nil {
		t.Fatalf("Message after the resume: %v", err)
	}
	if after, _ := manager.ActiveSession(); after == "" || after == before {
		t.Errorf("speech from after the resume went to session %q, want a new session, not the pre-pause session %s", after, before)
	}
}

// At shutdown a pause stops retrying a session end that keeps failing,
// instead of calling a closed database until the process exits.
func TestPauseStopsRetryingTheSessionEndAtShutdown(t *testing.T) {
	log := newCallLog()
	sessions := &fakeSessions{log: log, active: "s1", endErr: errors.New("sql: database is closed")}
	ctx, cancel := context.WithCancel(context.Background())
	capture := newSessionCaptureGate(ctx, sessions, fakePreroll{log}, func() deepgramFinalizer { return nil }, time.Hour, testEndRetry, nopLogf)

	capture.Pause()
	ends := func() int {
		return len(slices.DeleteFunc(log.get(), func(call string) bool { return call != "end-session" }))
	}
	for deadline := time.Now().Add(testWait); ends() < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the failed session end was not retried")
		}
	}
	cancel()
	capture.mu.Lock()
	settled := capture.settled
	capture.mu.Unlock()
	select {
	case <-settled:
	case <-time.After(testWait):
		t.Fatal("pause still retrying the session end after shutdown")
	}
	n := ends()
	time.Sleep(100 * time.Millisecond)
	if got := ends(); got != n {
		t.Fatalf("session end tried %d more times after the pause stopped at shutdown", got-n)
	}
}

type prerollFunc func()

func (f prerollFunc) DiscardPreroll() { f() }

// countingResumeGate is the capture gate with onResume run as each resume
// enters the gate (resumeWith).
type countingResumeGate struct {
	*captureGate
	onResume func()
}

func (g countingResumeGate) resumeWith(t resumeTicket, persist func() error) error {
	g.onResume()
	return g.captureGate.resumeWith(t, persist)
}

// TestGateOpenRefusalDoesNotDeadlockConcurrentResume fails within its timeout
// if the refusal callback runs while openAfter still holds g.control.
func TestGateOpenRefusalDoesNotDeadlockConcurrentResume(t *testing.T) {
	settled := make(chan struct{})
	capture := newCaptureGate(nil, func() { <-settled }, nil)
	store := &pauseStoreFake{}
	var resumeCalls atomic.Int32
	secondRequest := make(chan struct{})
	controls := newPersistentControls(store, capture.Pause, countingResumeGate{capture, func() {
		if resumeCalls.Add(1) == 2 {
			close(secondRequest)
		}
	}}, nil)
	gateCheckEntered := make(chan struct{})
	releaseGateCheck := make(chan struct{})
	var gateCheckRan atomic.Bool
	capture.beforeGateOpen = func() error {
		if !gateCheckRan.Swap(true) {
			close(gateCheckEntered)
			<-releaseGateCheck
			return errors.New("recording light stayed off")
		}
		return nil
	}
	refused := make(chan struct{})
	capture.onResumeRefused = func(error) {
		defer close(refused)
		if err := controls.RefuseResume(); err != nil {
			t.Errorf("persist refusal: %v", err)
		}
	}
	capture.Pause()
	if err := controls.Resume(); err != nil {
		t.Fatalf("first resume: %v", err)
	}
	close(settled)
	select {
	case <-gateCheckEntered:
	case <-time.After(testWait):
		t.Fatal("gate-open check did not start")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- controls.Resume() }()
	select {
	case <-secondRequest:
	case <-time.After(testWait):
		t.Fatal("second resume did not enter RequestResume")
	}
	close(releaseGateCheck)
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second resume: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("second resume deadlocked with gate-open refusal")
	}
	select {
	case <-refused:
	case <-time.After(testWait):
		t.Fatal("gate-open refusal callback deadlocked")
	}
}
