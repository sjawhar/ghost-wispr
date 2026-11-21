package voice

import (
	"runtime"
	"slices"
	"testing"
	"time"
)

// TestAHandLoweredDuringCloseIsNoRace fails, under the race detector, if a
// hand lowered while the Supervisor closes (LowerHands, which the roster
// calls from its own goroutines, and which spawns the hand's outcome) races
// Close's wait for the Supervisor's goroutines: with a sync.WaitGroup, an Add
// from zero while Wait runs is a data race. From review 6006769231 of #143,
// where joined's TestFloorTextIsCheckedWithItsAttribution hit it at
// GOMAXPROCS=8. The outcome of a hand lowered before Close found no
// goroutine running is still reported.
func TestAHandLoweredDuringCloseIsNoRace(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(8))
	for i := range 300 {
		r, outcomes := newHandsRig(t)
		h := testHand("h1", "ABC-12", time.Minute)
		if err := r.s.RaiseHand(h); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			r.s.Close()
		}()
		r.s.LowerHands(h.Membership)
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: Close did not return", i)
		}
		if got := outcomes.all(); len(got) > 1 || len(got) == 1 && !slices.Equal(got, []string{"h1:withdrawn"}) {
			t.Fatalf("run %d: outcomes %v, want at most h1 withdrawn", i, got)
		}
	}
}
