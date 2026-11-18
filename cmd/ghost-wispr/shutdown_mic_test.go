package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
)

// callMic is a capture device for the mic loop that records each call and
// whether its stream is open. Stream returns once Close is called, or at once
// on a closed device. onOpen, if set, runs at the end of each Open.
type callMic struct {
	mu     sync.Mutex
	events []string
	open   bool
	stop   chan struct{}
	onOpen func()
}

func newCallMic(open bool) *callMic { return &callMic{open: open, stop: make(chan struct{})} }

func (m *callMic) opened(event string) error {
	m.mu.Lock()
	m.events = append(m.events, event)
	m.open = true
	m.stop = make(chan struct{})
	onOpen := m.onOpen
	m.mu.Unlock()
	if onOpen != nil {
		onOpen()
	}
	return nil
}

func (m *callMic) Open() error   { return m.opened("open") }
func (m *callMic) Reopen() error { return m.opened("reopen") }

func (m *callMic) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, "start")
	return nil
}

func (m *callMic) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, "close")
	m.open = false
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	return nil
}

func (m *callMic) Stream(io.Writer) error {
	m.mu.Lock()
	m.events = append(m.events, "stream")
	open, stop := m.open, m.stop
	m.mu.Unlock()
	if !open {
		return errors.New("microphone stream unavailable")
	}
	<-stop
	return errors.New("microphone closed")
}

func (m *callMic) state() (bool, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.open, slices.Clone(m.events)
}

func (m *callMic) saw(event string) bool {
	_, events := m.state()
	return slices.Contains(events, event)
}

// TestTheMicLoopClosesItsMicrophoneWhenShutdownEndsIt fails if the mic loop
// ends at shutdown with its microphone still open. main turns the light off
// once joinMicLoops reports every loop ended, and closes the device only at
// TerminatePortAudio, after ForceEndSession (up to 30 s): the light would be
// off with the microphone open. The loop returned at its top without closing
// the device it held: on the first pass of a loop that a start shutdown
// overtook started, and after its own open for a resume (ReviewPR77 on #77,
// round 2, finding 3). The control: shutdown while the loop streams.
func TestTheMicLoopClosesItsMicrophoneWhenShutdownEndsIt(t *testing.T) {
	t.Run("control: shutdown while the loop streams", func(t *testing.T) {
		mic := newCallMic(true)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := goDone(func() {
			streamMicWithRetry(ctx, newCaptureGate(nil, nil, nil), mic, io.Discard, func(time.Duration) {}, nopLogf, nil)
		})
		waitForHardware(t, "the loop to stream", func() bool { return mic.saw("stream") })
		cancel()
		if !returnedWithin(done, testWait) {
			t.Fatal("the mic loop did not end at shutdown")
		}
		if open, events := mic.state(); open {
			t.Fatalf("control: the mic loop ended at shutdown with its microphone open: device calls %v", events)
		}
	})
	t.Run("the loop starts once shutdown has begun, with the start's microphone open", func(t *testing.T) {
		// startMicPipeline hands the open, started microphone to
		// startMicMonitoring; a SIGTERM during the start cancels ctx before
		// the loop's first step.
		mic := newCallMic(true)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		streamMicWithRetry(ctx, newCaptureGate(nil, nil, nil), mic, io.Discard, func(time.Duration) {}, nopLogf, nil)
		if open, events := mic.state(); open {
			t.Fatalf("the mic loop ended at shutdown with its microphone open: device calls %v", events)
		}
	})
	t.Run("shutdown during the loop's open for a resume", func(t *testing.T) {
		capture := newCaptureGate(nil, nil, nil)
		capture.Pause()
		mic := newCallMic(true)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mic.onOpen = cancel // SIGTERM while PortAudio opens the stream
		done := goDone(func() {
			streamMicWithRetry(ctx, capture, mic, io.Discard, func(time.Duration) {}, nopLogf, nil)
		})
		waitForHardware(t, "the loop to close the microphone for the pause", func() bool { return mic.saw("close") })
		if err := capture.RequestResume(); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if !returnedWithin(done, testWait) {
			t.Fatal("the mic loop did not end at shutdown")
		}
		if open, events := mic.state(); open {
			t.Fatalf("the mic loop ended at shutdown with its microphone open: device calls %v", events)
		}
	})
}
