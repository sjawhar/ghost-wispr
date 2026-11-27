// Sol's twenty-seventh review of #191 at 1d31fc1
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6089225486):
// Sol's probes, kept as regression tests, with five rows left out, each as Sol's
// review or Main ruled:
//   - TestSolR27ShorterCorrectionWithdrawsSameInterim's two bare "Whisper." rows,
//     which Sol found fail only their hold-flag expectation at both revisions: that
//     text is a call, which clears the interim and answers nothing;
//   - TestSolR27MisheardSpanEdges' ends_at_only_question_word,
//     overlap_contains_newer_question and missing_capture_times, which withdraw the
//     question at the baseline too, by the inclusive span rule of endpoints or, with
//     no capture times, by arrival; Main kept those outcomes.
// Every other row and check is Sol's. Since Sol's twenty-eighth review,
// r27Hear also gives each result its own start (Transcript.From) where its
// first word starts, as the live stream does; Sol's first-word-only results
// stay covered by TestAFirstWordStandsInForAMissingResultStart.

package voice

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

func r27Hear(r *rig, captured time.Time, text string, start float64, final, endpoint bool, speaker int) Transcript {
	tr := transcriptOf(text, start, final, endpoint, 0)
	for i := range tr.Words {
		tr.Words[i].Speaker = speaker
		tr.Words[i].At = captured.Add(time.Duration(tr.Words[i].Start * float64(time.Second)))
	}
	tr.From = captured.Add(time.Duration(start * float64(time.Second)))
	r.s.OnTranscript(&tr)
	return tr
}

func r27Open(t *testing.T) (*rig, *fakeConcierge, *fakeSession, time.Time) {
	t.Helper()
	c := newFakeConcierge()
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
	captured := time.Now().Add(-time.Minute)
	r27Hear(r, captured, "Whisper", 0, false, false, 3)
	sess := r.waitSession()
	r27Hear(r, captured, "Whisper. No. Oh, yeah,", 0, true, true, 3)
	r24HeldExchange(t, r)
	r27Hear(r, captured, "Wispr.", 5, false, false, 4)
	return r, c, sess, captured
}

func TestSolR27MisheardSpanEdges(t *testing.T) {
	for _, tc := range []struct {
		name                string
		question            string
		qStart, finalLength float64
		untimed             bool
		wantPreserved       bool
	}{
		{"strictly_older", "Can you send these notes?", 10, .15, false, true},
		{"ends_at_question_first_word", "Can you send these notes?", 5.95, .15, false, true},
		{"overlap_newer_tail", "Can you send these notes?", 5.8, .15, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, captured := r27Open(t)
			q := r27Hear(r, captured, tc.question, tc.qStart, false, false, 4)
			late := transcriptOf("Whisper networks are a problem.", 5, true, true, 0)
			for i := range late.Words {
				late.Words[i].Speaker = 9
				if !tc.untimed {
					late.Words[i].At = captured.Add(time.Duration(late.Words[i].Start * float64(time.Second)))
				}
			}
			last := &late.Words[len(late.Words)-1]
			last.End = last.Start + tc.finalLength
			r.s.mu.Lock()
			ex := r.s.ex
			before, beforeAsker := r.s.turnOf(ex), ex.x.AskerTurn
			heardTo := ex.heardTo
			r.s.mu.Unlock()
			if before != tc.question || beforeAsker != tc.question {
				t.Fatal("control: question not pending")
			}
			t.Logf("final end relative question start=%v; latest-question-word relative final end=%v; endsTurn=%v", late.endsAt().Sub(q.Words[0].At), heardTo.Sub(late.endsAt()), !heardTo.After(late.endsAt()))
			r.s.OnTranscript(&late)
			r.s.mu.Lock()
			after, asker, held := r.s.turnOf(ex), ex.x.AskerTurn, ex.misheardHold
			r.s.mu.Unlock()
			if tc.wantPreserved && (after != before || asker != beforeAsker || held) {
				t.Fatalf("independent question withdrawn: turn=%q AskerTurn=%q held=%v", after, asker, held)
			}
			r.s.OnUtteranceEnd(0, true, q.endsAt(), monoclock.Now())
			if got := r.waitTurn(); got != tc.question {
				t.Fatalf("question handed=%q", got)
			}
		})
	}
}

func TestSolR27LateRealCall(t *testing.T) {
	for _, sameSpeaker := range []bool{true, false} {
		t.Run(fmt.Sprint(sameSpeaker), func(t *testing.T) {
			r, _, _, captured := r27Open(t)
			q := r27Hear(r, captured, "Can you send these notes?", 10, false, false, 4)
			speaker := 4
			if !sameSpeaker {
				speaker = 9
			}
			late := r27Hear(r, captured, "Wispr, check the budget", 5, true, true, speaker)
			if !late.endsAt().Before(q.Words[0].At) {
				t.Fatal("control: final is not older")
			}
			r.s.mu.Lock()
			ex := r.s.ex
			pending, asker, held := r.s.turnOf(ex), ex.x.AskerTurn, ex.misheardHold
			r.s.mu.Unlock()
			if !strings.Contains(pending, "Can you send these notes?") || !strings.Contains(asker, "Can you send these notes?") || held {
				t.Errorf("late real call lost newer question: turn=%q AskerTurn=%q held=%v", pending, asker, held)
			}
			r.s.OnUtteranceEnd(0, true, q.endsAt(), monoclock.Now())
			if got := r.waitTurn(); !strings.Contains(got, "Can you send these notes?") {
				t.Fatalf("answer lost newer question: %q", got)
			}
		})
	}
}

func TestSolR27WithdrawalOrders(t *testing.T) {
	for _, endpoint := range []string{"utterance_end", "turn_timer"} {
		for _, order := range []string{"final_then_endpoint", "endpoint_then_final", "final_then_newer_interim_then_endpoint", "newer_interim_then_final_then_endpoint", "newer_interim_then_endpoint_then_final", "final_then_newer_final", "newer_final_then_final"} {
			t.Run(endpoint+"/"+order, func(t *testing.T) {
				r, c, _, captured := r27Open(t)
				r27Hear(r, captured, "Wispr, send these notes", 5, false, false, 4)
				correction := func() { r27Hear(r, captured, "Whisper networks are a problem.", 5, true, true, 9) }
				question := "Can you send the real report?"
				q := transcriptOf(question, 10, false, false, 0)
				for i := range q.Words {
					q.Words[i].At = captured.Add(time.Duration(q.Words[i].Start * float64(time.Second)))
					q.Words[i].Speaker = 4
				}
				interim := func() { r.s.OnTranscript(&q) }
				final := func() { r27Hear(r, captured, question, 10, true, true, 4) }
				end := func() {
					if endpoint == "utterance_end" {
						r.s.OnUtteranceEnd(0, true, q.endsAt(), monoclock.Now())
					} else {
						r.s.mu.Lock()
						ex := r.s.ex
						r.s.mu.Unlock()
						r.s.turnWaited(ex)
					}
				}
				want := ""
				switch order {
				case "final_then_endpoint":
					correction()
					end()
				case "endpoint_then_final":
					end()
					correction()
					want = "send these notes"
				case "final_then_newer_interim_then_endpoint":
					correction()
					interim()
					end()
				case "newer_interim_then_final_then_endpoint":
					interim()
					correction()
					end()
					want = question
				case "newer_interim_then_endpoint_then_final":
					interim()
					end()
					correction()
					want = question
				case "final_then_newer_final":
					correction()
					final()
					want = question
				case "newer_final_then_final":
					final()
					correction()
					want = question
				}
				if want != "" {
					if got := r.waitTurn(); got != want {
						t.Fatalf("handed=%q want=%q", got, want)
					}
				} else {
					r.s.mu.Lock()
					ex := r.s.ex
					held, words, answering := ex != nil && ex.misheardHold, "", ex != nil && ex.answering
					if ex != nil {
						words = r.s.turnOf(ex)
					}
					r.s.mu.Unlock()
					if !held || words != "" || answering {
						t.Fatalf("withdrawn request escaped: held=%v words=%q answering=%v", held, words, answering)
					}
					final()
					if got := r.waitTurn(); got != question {
						t.Fatalf("released question=%q", got)
					}
				}
				c.mu.Lock()
				count := len(c.turns)
				c.mu.Unlock()
				if count != 1 {
					t.Fatalf("answer count=%d", count)
				}
			})
		}
	}
}

func TestSolR27ShorterCorrectionWithdrawsSameInterim(t *testing.T) {
	for _, correction := range []string{"Whisper networks.", "Whisper. No. Wait,"} {
		for _, speaker := range []int{4, 9} {
			t.Run(fmt.Sprintf("%s/speaker%d", correction, speaker), func(t *testing.T) {
				c := newFakeConcierge()
				c.hold = true
				r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = 200 * time.Millisecond })
				captured := time.Now().Add(-time.Minute)
				r27Hear(r, captured, "Whisper", 0, false, false, 3)
				sess := r.waitSession()
				r27Hear(r, captured, "Whisper. No. Oh, yeah,", 0, true, true, 3)
				r24HeldExchange(t, r)
				interim := r27Hear(r, captured, "Wispr, send these notes", 5, false, false, 4)
				final := r27Hear(r, captured, correction, 5, true, true, speaker)
				if !final.endsAt().Before(interim.Words[len(interim.Words)-1].At) {
					t.Fatal("control: final did not shorten its own interim")
				}
				r.s.mu.Lock()
				ex := r.s.ex
				words, asker, held := r.s.turnOf(ex), ex.x.AskerTurn, ex.misheardHold
				r.s.mu.Unlock()
				if words != "" || asker != "" || !held {
					t.Errorf("same-span final failed to withdraw its interim: turn=%q AskerTurn=%q held=%v", words, asker, held)
				}
				r.s.OnUtteranceEnd(0, true, final.endsAt(), monoclock.Now())
				select {
				case got := <-c.asked:
					t.Fatalf("ordinary timer answered withdrawn words %q", got)
				case <-time.After(350 * time.Millisecond):
					if !closedNow(sess) {
						t.Fatal("misheard correction did not expire")
					}
				}
			})
		}
	}
}
