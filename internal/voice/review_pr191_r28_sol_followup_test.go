// Sol's twenty-eighth review of #191 at 37103b6
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6090659290):
// Sol's probes, kept as regression tests, unchanged: a late call in the follow-up window.

package voice

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

func TestSolR28LateCallInFollowUp(t *testing.T) {
	for _, mode := range []string{"ordered_call", "late_call", "no_call_control"} {
		t.Run(mode, func(t *testing.T) {
			c := newFakeConcierge("The issue is ready.")
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) {
				cfg.FollowUpWait, cfg.TurnWait = time.Hour, time.Hour
			})
			sess := r.ask("what is happening?")
			waitSaid(t, sess)
			r.speaker.finish(audio.InputVoice)
			r.waitEvent("voice_spoken:sess-1")
			eventually(t, "voice A's follow-up window", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.ex != nil && r.s.ex.followUp && !r.s.ex.answering
			})
			// Place both recordings after the answer and before delivery.
			capture := time.Now().Add(-20 * time.Second)
			time.Sleep(3 * time.Second)
			call := func() { r27Hear(r, capture, "Wispr, send these notes to Raj.", 20.35, true, false, 4) }
			if mode == "ordered_call" {
				call()
			}
			r27Hear(r, capture, "and to Lee.", 22, false, false, 4)
			if mode == "late_call" {
				call()
			}
			r.s.mu.Lock()
			ex := r.s.ex
			byName, follow, pending := ex.turnByName, ex.followUpTurn, r.s.turnOf(ex)
			r.s.mu.Unlock()
			t.Logf("mode=%s byName=%v followUpTurn=%v pending=%q", mode, byName, follow, pending)
			r27Hear(r, capture, "and to Lee.", 22, true, true, 4)
			if mode == "no_call_control" {
				eventually(t, "side talk closes its window", func() bool { return closedNow(sess) })
				select {
				case got := <-c.asked:
					t.Fatalf("side talk reached Answer: %q", got)
				default:
				}
				return
			}
			select {
			case got := <-c.asked:
				if !strings.Contains(got, "send these notes to Raj.") {
					t.Errorf("lost explicit request: %q", got)
				}
				t.Logf("Answer(%q)", got)
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("explicit call was not answered; closed=%v followUpTurn=%v byName=%v", closedNow(sess), follow, byName)
			}
		})
	}
}
