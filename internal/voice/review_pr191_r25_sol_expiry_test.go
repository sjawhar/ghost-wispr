// Sol's twenty-fifth review of #191 at 053303b
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6086871370):
// Sol's probes, kept as regression tests, unchanged: the withdrawn interim and a queued turn timer.

package voice

import (
	"testing"
	"time"
)

// Model the normal turn timer becoming runnable just before the withdrawn
// interim's final acquires the Supervisor lock. Stopping that timer cannot
// remove a callback which already waits for the lock.
func TestSolR25WithdrawnInterimQueuedTurnExpiry(t *testing.T) {
	for _, withdraw := range []bool{false, true} {
		name := "control_unwithdrawn_interim"
		if withdraw {
			name = "withdrawn_interim"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			officeWake(t, r)
			r.s.mu.Lock()
			ex := r.s.ex
			r.s.mu.Unlock()
			r.heardAs("Wispr, send these notes", false, false, 4)
			if withdraw {
				r.heardAs("Whisper networks are a problem.", true, true, 4)
			}
			r.s.turnWaited(ex)
			if !withdraw {
				if got := r.waitTurn(); got != "send these notes" {
					t.Fatalf("control handed %q", got)
				}
				return
			}
			r.s.mu.Lock()
			answering, held := ex.answering, ex.misheardHold
			r.s.mu.Unlock()
			if answering {
				t.Fatalf("queued ordinary turn expiry answered the withdrawn interim while held=%v", held)
			}
		})
	}
}
