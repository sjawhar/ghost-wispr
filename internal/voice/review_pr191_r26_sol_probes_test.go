// Sol's twenty-sixth review of #191 at b7ca361
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6088261337):
// Sol's probes, kept as regression tests, unchanged: the withdrawn-interim hold's release, expiry, speakers and stops.

package voice

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

func TestSolR26QuestionReleasesWithdrawnHold(t *testing.T) {
	for _, speaker := range []int{4, 9} {
		for _, endpoint := range []string{"speech_final", "utterance_end", "turn_timer"} {
			t.Run(fmt.Sprintf("speaker%d/%s", speaker, endpoint), func(t *testing.T) {
				r, c, sess, ex := r25WithdrawnHold(t)
				r.s.mu.Lock()
				gen := ex.holdGen
				r.s.mu.Unlock()
				r.s.turnWaited(ex) // an ordinary timer already queued during the hold
				r.heardAs("Can you send this report to Raj?", true, endpoint == "speech_final", speaker)
				switch endpoint {
				case "utterance_end":
					r.s.mu.Lock()
					end := ex.heardTo.Add(time.Millisecond)
					r.s.mu.Unlock()
					r.s.OnUtteranceEnd(0, true, end, monoclock.Now())
				case "turn_timer":
					r.s.turnWaited(ex)
				}
				if got := r.waitTurn(); got != "Can you send this report to Raj?" {
					t.Fatalf("actual question lost or polluted: %q", got)
				}
				r.s.holdExpired(ex, gen)
				r.s.turnWaited(ex)
				r.s.mu.Lock()
				live := r.s.ex == ex && !ex.ended && !ex.misheardHold && ex.answering
				r.s.mu.Unlock()
				if !live || closedNow(sess) {
					t.Fatal("old expiry ended the accepted question")
				}
				c.mu.Lock()
				count := len(c.turns)
				c.mu.Unlock()
				if count != 1 {
					t.Fatalf("Answer called %d times", count)
				}
			})
		}
	}
}

func TestSolR26ExpiryQuestionLinearization(t *testing.T) {
	for _, order := range []string{"question_first", "expiry_first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			for range 32 {
				c := newFakeConcierge()
				c.hold = true
				r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
				sess := officeWake(t, r)
				ex := r24HeldExchange(t, r)
				r.s.mu.Lock()
				gen := ex.holdGen
				r.s.mu.Unlock()
				question := func() { r.heardAs("Can you send the report?", true, true, 9) }
				expiry := func() { r.s.holdExpired(ex, gen) }
				switch order {
				case "question_first":
					question()
					expiry()
				case "expiry_first":
					expiry()
					question()
				default:
					start := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() { defer wg.Done(); <-start; question() }()
					go func() { defer wg.Done(); <-start; expiry() }()
					close(start)
					wg.Wait()
				}
				r.s.mu.Lock()
				answering, ended := ex.answering, ex.ended
				live := r.s.ex == ex
				r.s.mu.Unlock()
				if order == "question_first" && !answering {
					t.Fatal("question that won was lost")
				}
				if order == "expiry_first" && !ended {
					t.Fatal("expiry control did not close its hold")
				}
				if answering {
					if ended || !live || closedNow(sess) {
						t.Fatal("expiry closed an already accepted answer")
					}
					if got := r.waitTurn(); got != "Can you send the report?" {
						t.Fatalf("question=%q", got)
					}
					r.s.Stop("r26 cleanup")
				} else if !ended || live {
					t.Fatal("neither question nor expiry took effect")
				}
				eventually(t, "voice closes", func() bool { return closedNow(sess) })
				c.mu.Lock()
				count := len(c.turns)
				c.mu.Unlock()
				want := 0
				if answering {
					want = 1
				}
				if count != want {
					t.Fatalf("Answer count=%d want=%d", count, want)
				}
			}
		})
	}
}

func TestSolR26WithdrawalSpeakerAndAsker(t *testing.T) {
	for _, speaker := range []int{4, 9} {
		t.Run(fmt.Sprintf("corrected_final_speaker%d", speaker), func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			sess := officeWake(t, r)
			r.heardAs("Wispr, send these notes", false, false, 4)
			r.heardAs("Whisper networks are a problem.", true, true, speaker)
			ex := r24HeldExchange(t, r)
			r.s.mu.Lock()
			turn, asker, finals := r.s.turnOf(ex), ex.x.AskerTurn, ex.x.Finals
			r.s.mu.Unlock()
			if turn != "" || asker != "" || finals != "" {
				t.Fatalf("withdrawn words survive: %q / %q / %q", turn, asker, finals)
			}
			if room := sess.x.Room(); strings.Contains(room.Asker, "send these notes") || strings.Contains(room.Request, "send these notes") {
				t.Fatalf("withdrawn room=%+v", room)
			}
			r.heardAs("Can you send the real report?", true, true, 4)
			if got := r.waitTurn(); got != "Can you send the real report?" {
				t.Fatalf("real asker lost: %q", got)
			}
		})
		t.Run(fmt.Sprintf("different_time_final_speaker%d", speaker), func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			officeWake(t, r)
			r.heardAs("Wispr, send this report", true, false, 4)
			r.mu.Lock()
			start, closings := r.start, r.closings
			r.mu.Unlock()
			tr := transcriptOf("Whisper networks are a problem.", start, true, true, closings)
			for i := range tr.Words {
				tr.Words[i].Speaker = speaker
			}
			r.s.OnTranscript(&tr)
			if got := r.waitTurn(); !strings.HasPrefix(got, "send this report ") {
				t.Fatalf("independent final erased finalized asker: %q", got)
			}
		})
	}
}

func TestSolR26WithdrawalRetainsFinalizedAsker(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
	r.heardAs("Wispr, send this report", true, false, 4)
	sess := r.waitSession()
	r.s.mu.Lock()
	ex := r.s.ex
	start := ex.wakeStart
	r.s.mu.Unlock()
	r.heardAs("and its appendix", false, false, 4)
	late := transcriptOf("Whisper networks are a problem.", start, true, true, 0)
	for i := range late.Words {
		late.Words[i].Speaker = 9
	}
	r.s.OnTranscript(&late)
	r.s.mu.Lock()
	turn, asker, held := r.s.turnOf(ex), ex.x.AskerTurn, ex.misheardHold
	r.s.mu.Unlock()
	if !held || turn != "send this report" || asker != turn {
		t.Fatalf("finalized asker erased: hold=%v turn=%q asker=%q", held, turn, asker)
	}
	if room := sess.x.Room(); room.Asker != "send this report" {
		t.Fatalf("finalized room asker=%q", room.Asker)
	}
	r.heardAs("Can you include the summary?", true, true, 4)
	if got := r.waitTurn(); got != "send this report Can you include the summary?" {
		t.Fatalf("surviving request=%q", got)
	}
}

func TestSolR26StopDuringWithdrawnHold(t *testing.T) {
	for _, kind := range []string{"interim_stop", "final_stop", "button", "gate"} {
		t.Run(kind, func(t *testing.T) {
			r, c, sess, ex := r25WithdrawnHold(t)
			switch kind {
			case "interim_stop":
				r.heardAs("Wispr, stop", false, false, 9)
			case "final_stop":
				r.heardAs("Wispr, stop", true, true, 9)
			case "button":
				r.s.Stop("r26 stop")
			case "gate":
				r.closeGate()
			}
			if r24SameExchange(r, ex) || ex.ctx.Err() == nil {
				t.Fatal("stop was blocked by hold")
			}
			eventually(t, "stop closes held voice", func() bool { return closedNow(sess) })
			r.s.turnWaited(ex)
			r.s.holdExpired(ex, ex.holdGen)
			c.mu.Lock()
			n := len(c.turns)
			c.mu.Unlock()
			if n != 0 {
				t.Fatalf("stop handed %d answers", n)
			}
		})
	}
}
