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
// result on written. Every call to Stream, Start, Stop and Reopen is reported
// on events, in order.
type scriptedMic struct {
	chunks     chan []byte
	written    chan error
	events     chan string
	onStream   func()
	streamErrs []error
	startErrs  []error
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

func (m *scriptedMic) Stop() error {
	m.events <- "stop"
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
	t.Cleanup(func() { _, _ = recorder.EndSession() })
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

	mic.expectEvents(t, "stop")

	capture.Resume()
	mic.expectEvents(t, "start", "stream")
	mic.capture(t, make([]byte, 50))
	if got := deepgram.Len(); got != 150 {
		t.Fatalf("after resume: Deepgram got %d bytes in total, want 150", got)
	}
	if got := fileSize(t, recording); got != 150 {
		t.Fatalf("after resume: recording has %d bytes, want 150", got)
	}
}

// Pause stops the capture device itself, not only the audio after it, without
// treating the pause as a device error; resume starts the device again.
func TestPauseStopsMicDeviceUntilResume(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()
	deepgram := &lockedBuffer{}
	waits := &recordedWaits{}

	runMicLoop(t, capture, mic, deepgram, waits.wait, nil)
	mic.expectEvents(t, "stream")

	capture.Pause()
	mic.capture(t, []byte{1, 2, 3, 4})
	mic.expectEvents(t, "stop")
	mic.expectNoEvents(t)

	capture.Resume()
	mic.expectEvents(t, "start", "stream")
	mic.capture(t, []byte{5, 6})
	if got := deepgram.Len(); got != 2 {
		t.Fatalf("Deepgram got %d bytes, want only the 2 written after resume", got)
	}
	if got := waits.get(); len(got) != 0 {
		t.Fatalf("pause took the device-error backoff path, waits = %v", got)
	}
}

// The UI hears that the mic is paused, and that it is back on resume.
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
	mic.expectEvents(t, "stop")
	capture.Resume()
	mic.expectEvents(t, "start", "stream")

	var statuses []string
	deadline := time.After(testWait)
	for len(statuses) < 2 {
		select {
		case msg := <-events:
			var payload map[string]any
			if err := json.Unmarshal(msg, &payload); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if payload["type"] == "component_status" && payload["component"] == "mic" {
				statuses = append(statuses, fmt.Sprint(payload["status"]))
			}
		case <-deadline:
			t.Fatalf("timed out waiting for mic status events, got %v", statuses)
		}
	}
	if want := []string{storage.ComponentStatusPaused, storage.ComponentStatusConnected}; !slices.Equal(statuses, want) {
		t.Fatalf("mic statuses = %v, want %v", statuses, want)
	}
}

// A mic that comes up while capture is paused (the startup retry path) is
// stopped before any audio is read.
func TestMicStartedWhilePausedIsStoppedBeforeStreaming(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	mic := newScriptedMic()

	runMicLoop(t, capture, mic, io.Discard, func(time.Duration) {}, nil)
	mic.expectEvents(t, "stop")

	capture.Resume()
	mic.expectEvents(t, "start", "stream")
}

// Shutting down while paused ends the mic loop instead of leaving it waiting.
func TestPausedMicLoopStopsOnShutdown(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	mic := newScriptedMic()

	stop := runMicLoop(t, capture, mic, io.Discard, func(time.Duration) {}, nil)
	mic.expectEvents(t, "stop")
	if !stop() {
		t.Fatal("mic loop did not exit on shutdown while paused")
	}
	mic.expectNoEvents(t)
}

// A device error that coincides with a pause is not reported as a mic failure:
// the loop stops the device at once, with no backoff.
func TestDeviceErrorDuringPauseStopsMicWithoutBackoff(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()
	mic.onStream = capture.Pause
	mic.streamErrs = []error{errors.New("device disconnected")}
	waits := &recordedWaits{}

	runMicLoop(t, capture, mic, io.Discard, waits.wait, nil)
	mic.expectEvents(t, "stream", "stop")
	if got := waits.get(); len(got) != 0 {
		t.Fatalf("waits = %v, want none: a paused mic is stopped at once", got)
	}
}

// A device error near a pause (USB drop, reconnect backoff) must not reopen
// the mic while capture is paused; resume brings it back.
func TestPausedMicIsNotReopenedAfterDeviceError(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()
	mic.streamErrs = []error{errors.New("device disconnected")}
	waits := &recordedWaits{then: capture.Pause} // the pause arrives during the reconnect backoff
	deepgram := &lockedBuffer{}

	runMicLoop(t, capture, mic, deepgram, waits.wait, nil)
	mic.expectEvents(t, "stream", "stop")
	mic.expectNoEvents(t)

	capture.Resume()
	mic.expectEvents(t, "start", "stream")
	mic.capture(t, []byte{1, 2, 3})
	if got := deepgram.Len(); got != 3 {
		t.Fatalf("Deepgram got %d bytes after resume, want 3", got)
	}
	if got := waits.get(); len(got) != 1 || got[0] != time.Second {
		t.Fatalf("backoff waits = %v, want [1s]", got)
	}
}

// The input-overflow restart must not reopen the mic either when a pause
// arrives during its short wait.
func TestPausedMicIsNotReopenedAfterOverflow(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()
	mic.streamErrs = []error{errors.New("Input overflowed")}
	waits := &recordedWaits{then: capture.Pause}

	runMicLoop(t, capture, mic, io.Discard, waits.wait, nil)
	mic.expectEvents(t, "stream", "stop")

	capture.Resume()
	mic.expectEvents(t, "start", "stream")
}

// If the device cannot restart on resume (unplugged during the pause), the mic
// loop falls back to the reconnect path.
func TestResumeFallsBackToReopenWhenMicCannotRestart(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	mic := newScriptedMic()
	mic.startErrs = []error{errors.New("microphone stream unavailable")}
	waits := &recordedWaits{}

	runMicLoop(t, capture, mic, io.Discard, waits.wait, nil)
	mic.expectEvents(t, "stop")

	capture.Resume()
	mic.expectEvents(t, "start", "reopen", "stream")
	if got := waits.get(); len(got) != 1 || got[0] != time.Second {
		t.Fatalf("backoff waits = %v, want [1s]", got)
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
// later pause that has not settled yet.
func TestCaptureGateStaleResumeDoesNotOpenALaterPause(t *testing.T) {
	release := make(chan struct{})
	var settles atomic.Int32
	capture := newCaptureGate(nil, func() {
		if settles.Add(1) == 2 {
			<-release
		}
	}, nil)

	// Two Resumes wait on the first pause; one of them opens the gate.
	capture.Pause()
	capture.Resume()
	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)

	capture.Pause()
	capture.Resume()
	expectStaysClosed(t, capture, "before the second pause settled")
	close(release)
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
// a pause stops the device at once instead of after the backoff.
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

// storeSessions returns a session manager on a real SQLite store, which has
// one database connection.
func storeSessions(t *testing.T) (*storage.SQLiteStore, *session.Manager) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
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
// the session without it, leaving the raw audio on disk for recovery by hand,
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
