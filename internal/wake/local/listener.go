package local

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Capture is the microphone stream the listener reads: an ALSA capture in
// Ghost Wispr's own process, never PortAudio, at SampleRate, mono.
type Capture interface {
	// Read fills pcm with the next samples, blocking for about one period.
	Read(pcm []int16) (int, error)
	Close() error
}

// ErrNotNow is an Open's refusal that is no fault: the unit is not listening
// any more, or the recording microphone is open. The listener looks again.
var ErrNotNow = errors.New("the paused listener may not open now")

// ListenerConfig is what the listener needs from the unit.
type ListenerConfig struct {
	// Open opens the capture stream, only while Want holds and the recording
	// microphone is closed, or returns ErrNotNow.
	Open func() (Capture, error)
	// Want reports whether the unit is in the listening state.
	Want func() bool
	// Heard is the listener's only output: a resume request, which the unit
	// serves as POST /api/resume does. The capture is closed when it runs.
	Heard func()
	// Failed reports a capture that would not open, or failed while it read:
	// nothing listens, so the unit closes the microphone's state.
	Failed func(error)
	// Poll is how often the listener looks at Want while it does not listen;
	// 0 means 100 ms.
	Poll time.Duration
}

// periodSamples is one read: 100 ms. A Want that turns false or an Interrupt
// closes the stream within about one period.
const periodSamples = SampleRate / 10

// openTries is how many opens in a row may fail, Poll apart, before the
// listener reports the failure: the recording microphone's device can take a
// moment to release after its close.
const openTries = 5

// Listener holds the microphone for the detector while the unit listens, and
// for nothing else.
type Listener struct {
	detector    *Detector
	openCapture func() (Capture, error)
	want        func() bool
	heard       func()
	failed      func(error)
	poll        time.Duration
	// interrupted asks the open stream to close at its next read (Interrupt).
	interrupted atomic.Bool
	// mu guards open and the detector: the detector runs only under it.
	mu   sync.Mutex
	open bool
	buf  []int16
}

// NewListener makes a listener on detector.
func NewListener(detector *Detector, cfg ListenerConfig) *Listener {
	poll := cfg.Poll
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	return &Listener{
		detector:    detector,
		openCapture: cfg.Open,
		want:        cfg.Want,
		heard:       cfg.Heard,
		failed:      cfg.Failed,
		poll:        poll,
		buf:         make([]int16, periodSamples),
	}
}

// Interrupt asks an open stream to close at its next read, and returns at
// once: the capture device waits for the close before the recording
// microphone opens or PortAudio scans.
func (l *Listener) Interrupt() { l.interrupted.Store(true) }

// Run listens while Want holds, until ctx ends.
func (l *Listener) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		if !l.want() {
			failures = 0
			l.sleep(ctx)
			continue
		}
		l.interrupted.Store(false)
		capture, err := l.openCapture()
		switch {
		case errors.Is(err, ErrNotNow):
			l.sleep(ctx)
			continue
		case err != nil:
			failures++
			if failures >= openTries {
				failures = 0
				slog.Warn("paused listener: the capture did not open", "error", err)
				l.failed(err)
			}
			l.sleep(ctx)
			continue
		}
		failures = 0
		heard, readErr := l.listen(ctx, capture)
		if err := capture.Close(); err != nil {
			slog.Warn("paused listener: close the capture", "error", err)
		}
		l.reset()
		switch {
		case readErr != nil:
			slog.Warn("paused listener: the capture failed", "error", readErr)
			l.failed(readErr)
		case heard:
			slog.Info("paused listener: heard the phrase; asking to resume")
			l.heard()
		}
	}
}

// listen feeds the stream to the detector until the phrase is heard, Want
// turns false, Interrupt asks, ctx ends or a read fails.
func (l *Listener) listen(ctx context.Context, capture Capture) (heard bool, err error) {
	l.mu.Lock()
	l.open = true
	l.mu.Unlock()
	for ctx.Err() == nil && !l.interrupted.Load() && l.want() {
		n, err := capture.Read(l.buf)
		if err != nil {
			return false, err
		}
		l.mu.Lock()
		heard := l.detector.Write(l.buf[:n])
		l.mu.Unlock()
		if heard {
			return true, nil
		}
	}
	return false, nil
}

// reset zeroes the ring, the spotter's stream and the read buffer once the
// stream has closed: every state change starts the detector from silence.
func (l *Listener) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.detector.Reset()
	clear(l.buf)
	l.open = false
}

func (l *Listener) sleep(ctx context.Context) {
	timer := time.NewTimer(l.poll)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
