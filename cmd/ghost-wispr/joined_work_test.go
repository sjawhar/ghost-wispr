package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/stopping"
)

// joinedWork admits work as the session manager does (backgroundWork): Go
// and Run run it while joinedWork is not stopped, and refuse it once it is.
// A fixture gives it to what starts work on goroutines of its own, such as a
// capture gate's openings and the paused listener, and stops it in its
// cleanup before it restores a hook that work reads (openBlink1). Admitting
// and refusing share one lock, so no work starts after stop returns.
//
// stop does not end the work: the work must end once its context does,
// which the fixture cancels first. A stop called from inside admitted work
// would wait for itself; it, like work that does not end with its context,
// waits at most bound (testWait when 0), then fails t, naming both causes,
// and returns instead of hanging.
type joinedWork struct {
	ctx   context.Context // the context Go's work runs under
	t     testing.TB
	bound time.Duration

	mu      sync.Mutex
	stopped bool
	running sync.WaitGroup
}

func (w *joinedWork) admit() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false
	}
	w.running.Add(1)
	return true
}

func (w *joinedWork) Go(f func(ctx context.Context)) {
	if w.admit() {
		go func() {
			defer w.running.Done()
			f(w.ctx)
		}()
	}
}

func (w *joinedWork) Run(ctx context.Context, f func(ctx context.Context) error) error {
	if !w.admit() {
		return stopping.ErrStopped
	}
	defer w.running.Done()
	return f(ctx)
}

// stop refuses more work and waits for the work it admitted, at most bound.
func (w *joinedWork) stop() {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
	done := make(chan struct{})
	go func() {
		w.running.Wait()
		close(done)
	}()
	bound := w.bound
	if bound == 0 {
		bound = testWait
	}
	select {
	case <-done:
	case <-time.After(bound):
		w.t.Errorf("joinedWork.stop waited %v for the work it admitted to end: a stop called from inside that work waits for itself, and work that does not end with its context never ends", bound)
	}
}

// TestJoinedWorkWaitsForWhatItAdmitted fails if joinedWork's stop returns
// before the Go and Run work it admitted has ended, runs work admitted after
// it, or hangs when called from inside admitted work instead of failing the
// test with a message.
func TestJoinedWorkWaitsForWhatItAdmitted(t *testing.T) {
	t.Run("stop waits for Go and Run work", func(t *testing.T) {
		w := &joinedWork{ctx: t.Context(), t: t}
		var goEnded, runEnded bool
		var mu sync.Mutex
		started := make(chan struct{}, 2)
		w.Go(func(context.Context) {
			started <- struct{}{}
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			goEnded = true
			mu.Unlock()
		})
		go func() {
			_ = w.Run(t.Context(), func(context.Context) error {
				started <- struct{}{}
				time.Sleep(100 * time.Millisecond)
				mu.Lock()
				runEnded = true
				mu.Unlock()
				return nil
			})
		}()
		<-started
		<-started
		w.stop()
		mu.Lock()
		defer mu.Unlock()
		if !goEnded || !runEnded {
			t.Fatalf("stop returned with Go's work ended %v and Run's %v; want both ended", goEnded, runEnded)
		}
	})
	t.Run("no work runs once stopped", func(t *testing.T) {
		w := &joinedWork{ctx: t.Context(), t: t}
		w.stop()
		ran := false
		w.Go(func(context.Context) { ran = true })
		if err := w.Run(t.Context(), func(context.Context) error { ran = true; return nil }); err != stopping.ErrStopped || ran {
			t.Fatalf("after stop, Run = %v and work ran %v; want stopping.ErrStopped and no work", err, ran)
		}
	})
	t.Run("a stop from inside its own work fails the test instead of hanging", func(t *testing.T) {
		failed := &errorsOnly{TB: t}
		w := &joinedWork{ctx: t.Context(), t: failed, bound: 100 * time.Millisecond}
		returned := make(chan struct{})
		go func() {
			_ = w.Run(t.Context(), func(context.Context) error {
				w.stop()
				return nil
			})
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(testWait):
			t.Fatal("a stop called from inside admitted work hung")
		}
		if failed.message() == "" {
			t.Fatal("a stop called from inside admitted work returned without failing the test")
		}
	})
}

// errorsOnly is a testing.TB whose Errorf only records its message.
type errorsOnly struct {
	testing.TB
	mu  sync.Mutex
	msg string
}

func (e *errorsOnly) Errorf(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.msg = format
}

func (e *errorsOnly) message() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.msg
}
