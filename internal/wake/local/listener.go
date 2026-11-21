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

// ListenerConfig is what the listener needs from the unit. Each entry into the
// listening state starts a new stretch of listening, numbered by the unit: a
// stream, and every phrase or failure that comes of it, belongs to the
// stretch it was opened in, so the unit can refuse those of a stretch that has
// ended (review 5998292452 of #123).
type ListenerConfig struct {
	// Open opens the capture stream for stretch, only while the unit listens
	// in stretch and the recording microphone is closed, or returns ErrNotNow.
	Open func(stretch uint64) (Capture, error)
	// Want reports the unit's current stretch, and whether the listener
	// should hold the microphone in it.
	Want func() (stretch uint64, wanted bool)
	// Heard is the listener's only output: a resume request for a phrase
	// heard in stretch, which the unit serves as POST /api/resume does, and
	// only while it still listens in that stretch. The capture is closed when
	// it runs.
	Heard func(stretch uint64)
	// Failed reports a capture of stretch that would not open, or failed
	// while it read: nothing listens, so the unit closes the microphone's
	// state if it still listens in that stretch.
	Failed func(stretch uint64, err error)
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
	openCapture func(stretch uint64) (Capture, error)
	want        func() (uint64, bool)
	heard       func(stretch uint64)
	failed      func(stretch uint64, err error)
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
	failures, failingStretch := 0, uint64(0)
	for ctx.Err() == nil {
		stretch, wanted := l.want()
		if !wanted {
			failures = 0
			l.sleep(ctx)
			continue
		}
		if stretch != failingStretch {
			failures, failingStretch = 0, stretch
		}
		l.interrupted.Store(false)
		capture, err := l.openCapture(stretch)
		switch {
		case errors.Is(err, ErrNotNow):
			l.sleep(ctx)
			continue
		case err != nil:
			failures++
			if failures >= openTries {
				failures = 0
				slog.Warn("paused listener: the capture did not open", "error", err)
				l.failed(stretch, err)
			}
			l.sleep(ctx)
			continue
		}
		failures = 0
		heard, readErr := l.listen(ctx, capture, stretch)
		if err := capture.Close(); err != nil {
			slog.Warn("paused listener: close the capture", "error", err)
		}
		l.reset()
		switch {
		case readErr != nil:
			slog.Warn("paused listener: the capture failed", "error", readErr)
			l.failed(stretch, readErr)
		case heard:
			slog.Info("paused listener: heard the phrase; asking to resume")
			l.heard(stretch)
		}
	}
}

// listen feeds the stream of stretch to the detector until the phrase is
// heard, the unit stops listening or starts another stretch, Interrupt asks,
// ctx ends or a read fails.
func (l *Listener) listen(ctx context.Context, capture Capture, stretch uint64) (heard bool, err error) {
	l.mu.Lock()
	l.open = true
	l.mu.Unlock()
	for ctx.Err() == nil && !l.interrupted.Load() && l.inStretch(stretch) {
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

// inStretch reports whether the unit still wants the listener in stretch.
func (l *Listener) inStretch(stretch uint64) bool {
	current, wanted := l.want()
	return wanted && current == stretch
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
