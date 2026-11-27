package voice

import (
	"context"
	"strings"
	"testing"
)

// testTakeBack is a minimal whole-turn take-back rule for the Supervisor's
// count test: a final line of "never mind" takes the turn back, and the rest
// is whatever follows it.
func testTakeBack(finals string) (string, bool) {
	lines := strings.Split(finals, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "never mind" {
			return strings.TrimSpace(strings.Join(lines[i+1:], "\n")), true
		}
	}
	return "", false
}

// TestATakeBackCountsOnceDespiteADuplicateInterim (finding 5, Sol's
// eighteenth review of #191): the take-back is counted on the final with its
// obsolete interim removed first, so one "never mind" that arrived as an
// interim and then a final does not count twice when a later unrelated final
// ("okay, sure") shortens the saved cutoff. turnLocked is driven directly, as
// the Supervisor's own probe does, so the driving-turn reset does not hide the
// duplicate. At 377b117 the counter went from 1 to 2. Control: no interim.
func TestATakeBackCountsOnceDespiteADuplicateInterim(t *testing.T) {
	for _, interim := range []bool{true, false} {
		name := "no interim"
		if interim {
			name = "duplicate interim"
		}
		t.Run(name, func(t *testing.T) {
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				cfg.TakeBack = testTakeBack
			})
			ex := &exchange{ctx: context.Background(), times: &exchangeTimes{}, notified: map[string]bool{}, drives: true}
			feed := func(text string, final bool) {
				tr := transcriptOf(text, 0, final, final, 0)
				r.s.mu.Lock()
				r.s.turnLocked(ex, &tr, text)
				r.s.mu.Unlock()
			}
			if interim {
				feed("never mind", false) // the obsolete interim
			}
			feed("never mind", true) // the final take-back
			if ex.takeBacks != 1 {
				t.Fatalf("take-backs %d after the final, want 1", ex.takeBacks)
			}
			feed("okay, sure", true) // an unrelated later final
			if ex.takeBacks != 1 {
				t.Errorf("take-backs %d after the later final, want 1: the one take-back counted twice", ex.takeBacks)
			}
		})
	}
}
