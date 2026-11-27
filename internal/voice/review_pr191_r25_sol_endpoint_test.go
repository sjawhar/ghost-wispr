// Sol's twenty-fifth review of #191 at 053303b
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6086871370):
// Sol's probes, kept as regression tests, unchanged: the withdrawn interim and its UtteranceEnd.

package voice

import (
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

func TestSolR25WithdrawnInterimUtteranceEnd(t *testing.T) {
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
			r.mu.Lock()
			start, closings := r.start, r.closings
			r.mu.Unlock()
			captured := time.Now().Add(-2 * time.Second)
			hear := func(text string, final bool) Transcript {
				tr := transcriptOf(text, start, final, final, closings)
				for i := range tr.Words {
					tr.Words[i].At = captured.Add(time.Duration(i) * 200 * time.Millisecond)
				}
				r.s.OnTranscript(&tr)
				return tr
			}
			tr := hear("Wispr, send these notes", false)
			if withdraw {
				tr = hear("Whisper networks are a problem.", true)
			}
			if !tr.endsAt().Before(time.Now()) {
				t.Fatal("endpoint control: capture timestamp is not in the past")
			}
			r.s.OnUtteranceEnd(closings, true, tr.endsAt(), monoclock.Now())
			if !withdraw {
				if got := r.waitTurn(); got != "send these notes" {
					t.Fatalf("control handed %q", got)
				}
				return
			}
			select {
			case got := <-c.asked:
				t.Fatalf("UtteranceEnd handed withdrawn interim %q to Answer", got)
			case <-time.After(100 * time.Millisecond):
				r.s.mu.Lock()
				answering := r.s.ex != nil && r.s.ex.answering
				r.s.mu.Unlock()
				if answering {
					t.Fatal("UtteranceEnd began answering the withdrawn interim")
				}
			}
		})
	}
}
