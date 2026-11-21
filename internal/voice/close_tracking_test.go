package voice

import (
	"testing"
	"time"
)

// TestCloseWaitsForTheGoroutinesStartedBeforeItFoundNone fails if Close
// returns while a goroutine of the Supervisor's still runs, one started
// before Close began or one started while Close waited for another; or if a
// goroutine started once Close found none running does not run.
func TestCloseWaitsForTheGoroutinesStartedBeforeItFoundNone(t *testing.T) {
	r := newRig(t, newFakeConcierge())
	first, second := make(chan struct{}), make(chan struct{})
	r.s.spawn(func() { <-first })
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		r.s.Close()
	}()
	eventually(t, "Close waiting", func() bool {
		r.s.spawnMu.Lock()
		defer r.s.spawnMu.Unlock()
		return r.s.closing
	})
	r.s.spawn(func() { <-second }) // started while Close waits for the first
	close(first)
	select {
	case <-closed:
		t.Fatal("Close returned while a goroutine started during its wait still ran")
	case <-time.After(100 * time.Millisecond):
	}
	close(second)
	select {
	case <-closed:
	case <-time.After(closeWait + time.Second):
		t.Fatal("Close did not return once its goroutines ended")
	}
	ran := make(chan struct{})
	r.s.spawn(func() { close(ran) }) // Close found none running: untracked
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("a goroutine started after Close found none running did not run")
	}
}
