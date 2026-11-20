package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/wake/local"
)

// The paused listener's audio belongs to the stretch of listening it was
// captured in. A change of state while the listener reads or decodes, a close,
// the setting turned off, the Jabra detached, or a new stretch of listening,
// must win over a phrase or a failure that the old stretch's audio brings
// afterwards: nothing from it may resume recording or close a newer stretch
// (review of #123: a wake read before a close resumed after it).

// staleCapture is the listener's capture. The first stream's first read,
// when the rig holds it, blocks until release and then returns its frame:
// the phrase, or failRead's error. Every other read returns 10 ms of silence.
type staleCapture struct {
	r     *staleRig
	first bool
}

func (c staleCapture) Read(pcm []int16) (int, error) {
	if c.first && c.r.holdRead {
		c.r.once.Do(func() {
			close(c.r.reading)
			<-c.r.release
		})
		if c.r.failRead {
			return 0, errors.New("the capture failed")
		}
		for i := range pcm {
			pcm[i] = phraseSample
		}
		return len(pcm), nil
	}
	if c.first && !c.r.holdRead && c.r.phraseSent.CompareAndSwap(false, true) {
		for i := range pcm {
			pcm[i] = phraseSample
		}
		return len(pcm), nil
	}
	time.Sleep(10 * time.Millisecond)
	clear(pcm)
	return len(pcm), nil
}

func (staleCapture) Close() error { return nil }

// holdSpotter is stage 1 on phraseSample. When the rig holds stage 1, its
// first hit blocks until release: the listener has read the phrase and is
// deciding about it.
type holdSpotter struct{ r *staleRig }

func (s holdSpotter) Accept(samples []float32) bool {
	hit := slices.Contains(samples, float32(phraseSample)/32768)
	if hit && s.r.holdHit {
		s.r.once.Do(func() {
			close(s.r.reading)
			<-s.r.release
		})
	}
	return hit
}

func (holdSpotter) Reset() {}
func (holdSpotter) Close() {}

type staleRig struct {
	gate     *captureGate
	controls *persistentControls
	store    *lockedPauseStore
	// holdRead blocks the first read; holdHit blocks stage 1's first hit.
	holdRead, holdHit bool
	failRead          bool // the held read fails instead of giving the phrase
	once              sync.Once
	reading, release  chan struct{}
	phraseSent        atomic.Bool
	opens             atomic.Int32
	done              chan struct{}
	cancel            context.CancelFunc
}

// startStaleRig runs the real listener wiring (runPausedListener) with the
// real capture gate, persistent controls and capture device, in listening.
func startStaleRig(t *testing.T, holdRead, holdHit, failRead bool) *staleRig {
	t.Helper()
	r := &staleRig{holdRead: holdRead, holdHit: holdHit, failRead: failRead,
		reading: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	r.gate = newCaptureGate(nil, nil, nil)
	r.store = &lockedPauseStore{}
	device := newCaptureDevice(r.gate.inClosedState)
	device.attach(func(bool) error { return nil })
	r.controls = newPersistentControls(r.store, r.gate.Pause, r.gate, nil)
	r.controls.useListener(r.gate, func() {}, func() bool { return true }, device.closeListener)
	if err := r.controls.Pause(); err != nil {
		t.Fatal(err)
	}
	if got := r.gate.micState(); got != micListening {
		t.Fatalf("control: the rig starts in %v, want listening", got)
	}
	unit := &pausedListenerUnit{
		capture: r.gate, controls: r.controls, device: device,
		warnings: newWarningsRegistry(), wake: func() {}, built: true,
		hooks: &server.ControlHooks{Resume: r.controls.Resume},
		newDetector: func() (*local.Detector, error) {
			return local.NewDetectorOf(holdSpotter{r: r}, startVerifier{}), nil
		},
		openCapture: func() (local.Capture, error) {
			return staleCapture{r: r, first: r.opens.Add(1) == 1}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	var started atomic.Bool
	go func() {
		defer close(r.done)
		runPausedListener(ctx, unit, &started, func() {})
	}()
	t.Cleanup(func() {
		r.releaseOnce()
		cancel()
		select {
		case <-r.done:
		case <-time.After(testWait):
			t.Error("the listener did not stop")
		}
	})
	return r
}

func (r *staleRig) releaseOnce() {
	select {
	case <-r.release:
	default:
		close(r.release)
	}
}

// held waits for the held read or stage-1 hit.
func (r *staleRig) held(t *testing.T) {
	t.Helper()
	select {
	case <-r.reading:
	case <-time.After(testWait):
		t.Fatal("the listener did not reach the held read or stage-1 hit")
	}
}

// newStretch starts a new stretch of listening from closed, as a saved
// listening state does once the listener can run.
func (r *staleRig) newStretch(t *testing.T) {
	t.Helper()
	r.controls.armRestoredListening()
	if err := r.controls.listenerChanged(); err != nil {
		t.Fatal(err)
	}
	if got := r.gate.micState(); got != micListening {
		t.Fatalf("control: the new stretch is %v, want listening", got)
	}
}

// stays fails if the unit leaves want at any time within d.
func (r *staleRig) stays(t *testing.T, want micState, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := r.gate.micState(); got != want {
			t.Fatalf("%s: the unit went to %v, want it to stay %v (saved %q)", what, got, want, r.saved())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *staleRig) reaches(t *testing.T, want micState, what string) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for r.gate.micState() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: the unit is %v, want %v", what, r.gate.micState(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *staleRig) saved() string {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return r.store.values[storage.AppStateCapturePaused]
}

// TestAPhraseFromAnEndedStretchResumesNothing fails if the start phrase, read
// or decoded in a stretch of listening, resumes recording once that stretch
// has ended: closed by POST /api/close while the listener's read blocks
// (review of #123), closed while stage 1 decides about the phrase, or closed
// and followed by a new stretch of listening, which a check that the unit is
// listening cannot tell from the old one. Control: with no change of state,
// the same phrase resumes, so the rig hears it.
func TestAPhraseFromAnEndedStretchResumesNothing(t *testing.T) {
	for _, tc := range []struct {
		name              string
		holdRead, holdHit bool
		change            func(t *testing.T, r *staleRig)
		want              micState
	}{
		{"a close during the read", true, false, func(t *testing.T, r *staleRig) {
			if err := r.controls.Close(); err != nil {
				t.Fatal(err)
			}
		}, micClosed},
		{"a close while stage 1 decides", false, true, func(t *testing.T, r *staleRig) {
			if err := r.controls.Close(); err != nil {
				t.Fatal(err)
			}
		}, micClosed},
		{"a close and a new stretch while stage 1 decides", false, true, func(t *testing.T, r *staleRig) {
			if err := r.controls.Close(); err != nil {
				t.Fatal(err)
			}
			r.newStretch(t)
		}, micListening},
		{"control: no change during the read", true, false, nil, micRecording},
		{"control: no change while stage 1 decides", false, true, nil, micRecording},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startStaleRig(t, tc.holdRead, tc.holdHit, false)
			r.held(t)
			if tc.change != nil {
				tc.change(t, r)
			}
			before := r.saved()
			r.releaseOnce()
			if tc.want == micRecording {
				r.reaches(t, micRecording, "the phrase with no change of state")
				return
			}
			r.stays(t, tc.want, 300*time.Millisecond, "the phrase after "+tc.name)
			if after := r.saved(); after != before {
				t.Fatalf("the stale phrase changed the saved state from %q to %q", before, after)
			}
		})
	}
}

// TestAFailureFromAnEndedStretchClosesNothing fails if the listener's read
// failure from a stretch of listening that has ended closes the stretch that
// followed it. Control: with no change of state, the failure closes the
// microphone, since nothing listens.
func TestAFailureFromAnEndedStretchClosesNothing(t *testing.T) {
	t.Run("a close and a new stretch during the read", func(t *testing.T) {
		r := startStaleRig(t, true, false, true)
		r.held(t)
		if err := r.controls.Close(); err != nil {
			t.Fatal(err)
		}
		r.newStretch(t)
		r.releaseOnce()
		r.stays(t, micListening, 300*time.Millisecond, "the old stretch's read failure")
	})
	t.Run("control: no change during the read", func(t *testing.T) {
		r := startStaleRig(t, true, false, true)
		r.held(t)
		r.releaseOnce()
		r.reaches(t, micClosed, "a read failure in the current stretch")
	})
}
