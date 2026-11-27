package concierge

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Sol's nineteenth review of #191 at 8c192ba
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6076963198),
// through the real Supervisor, Concierge and GPT-Live client on a loopback
// vendor. A delegation's question is the room's own request, owned by final
// identity; a vendor delegation only asks to look at it (Main, after the
// review, and the voice B delegation design note, section 4).

// TestALocalFinalAfterTheDelegationIsAsked (findings 1 and 2, Sol's
// TestSol191R19LocalFinalAfterDelegation): GPT-Live delegates a real
// follow-up before the Pi's final of it arrives. At 8c192ba an old, nonempty
// asker was asked again at once (finding 1), and a final later than two
// seconds was dropped for good (finding 2). The final's arrival schedules the
// delegation's work once, with no deadline and no second vendor delegation.
// Rows: the final before the delegation (the control), 150 ms after it and
// 2,300 ms after it, each with and without a take-back first.
func TestALocalFinalAfterTheDelegationIsAsked(t *testing.T) {
	for _, cut := range []bool{false, true} {
		for _, delay := range []time.Duration{0, 150 * time.Millisecond, 2300 * time.Millisecond} {
			t.Run(fmt.Sprintf("takeback_%v/local_final_delay_%v", cut, delay), func(t *testing.T) {
				r, v, w := openVoiceB(t)
				if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
					t.Fatalf("initial question %q", q)
				}
				if cut {
					r.hearNow("never mind", false)
				}
				const fresh = "when will ABC-12 ship?"
				if delay == 0 {
					r.hearNow(fresh, false)
				}
				// Both transcribers report the same real question; the Pi's
				// final is merely later than GPT-Live's delegation.
				w.send(map[string]any{"type": "session.input_transcript.delta", "event_id": "fresh", "delta": fresh, "start_ms": 1000, "end_ms": 1100})
				sendDelegation(w, "followup", 2000)
				if delay > 0 {
					time.Sleep(delay)
					select {
					case c := <-v.calls:
						t.Fatalf("the tool ran before the room's final: %s", c.Args)
					default:
					}
					r.hearNow(fresh, false)
				}
				q := toolWithin(t, v, 2*time.Second)
				if q != fresh {
					t.Errorf("real room follow-up lost: tool got %q, want %q", q, fresh)
				}
				if q == fresh && modelQuestion(r) != fresh {
					t.Errorf("model got %q, want %q", modelQuestion(r), fresh)
				}
				select {
				case c := <-v.calls:
					t.Errorf("the late final made a second tool call: %s", c.Args)
				case <-time.After(300 * time.Millisecond):
				}
			})
		}
	}
}

// TestAReplacementAfterATakeBackIsAskedWhole (Sol's
// TestSol191R19SamePrefixAfterTakeback): a take-back and its replacement in
// one final. The replacement repeats the canceled question's words; it is
// asked whole, never cut by a prefix of the question before.
func TestAReplacementAfterATakeBackIsAskedWhole(t *testing.T) {
	for _, replacement := range []string{"what about ABC-12?", "what about ABC-42? and is ABC-12 merged?"} {
		t.Run(replacement, func(t *testing.T) {
			r, v, w := openVoiceB(t)
			if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
				t.Fatalf("initial question %q", q)
			}
			r.hearNow("never mind. "+replacement, false)
			if q, _ := delegate(t, w, v, r, "replacement", 2000); q != replacement {
				t.Errorf("replacement after take-back was truncated: %q, want %q", q, replacement)
			}
		})
	}
}

// TestANewTurnThatRepeatsThePrefixIsAskedWhole (finding 3, Sol's
// TestSol191R19NewTurnSamePrefix): an endpoint ends the first turn, and the
// next local turn repeats the first question and adds one. At 8c192ba the
// prefix rule cut the repeated question away; ownership by turn and final
// keeps the whole new turn.
func TestANewTurnThatRepeatsThePrefixIsAskedWhole(t *testing.T) {
	r, v, w := openVoiceB(t)
	if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
		t.Fatalf("initial question %q", q)
	}
	r.hearNow("okay", true) // a speech_final ends the driving turn
	const fresh = "what about ABC-42? and is ABC-12 merged?"
	r.hearNow(fresh, true)
	q, _ := delegate(t, w, v, r, "new-turn", 2000)
	if q != fresh {
		t.Errorf("new turn %q reached the tool as %q", fresh, q)
	}
	if !strings.Contains(modelQuestion(r), "ABC-42") {
		t.Errorf("the model lost the new turn's first question: %q", modelQuestion(r))
	}
}

// TestDelegationWaitsForOwnedFinalWithoutDeadline (the design note's proposed
// test, section 7.2): a delegation with no request in the room waits for the
// final it needs however late it comes, here well past the two seconds that
// dropped it at 8c192ba, and runs the tool exactly once after it, none
// before. Control: the exchange ends before the delayed final, and no call
// follows its end.
func TestDelegationWaitsForOwnedFinalWithoutDeadline(t *testing.T) {
	for _, ends := range []bool{false, true} {
		name := "the final comes 3.5 s after the delegation"
		if ends {
			name = "control: the exchange ends before the final"
		}
		t.Run(name, func(t *testing.T) {
			r, v, w := openVoiceB(t)
			if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
				t.Fatalf("initial question %q", q)
			}
			sendDelegation(w, "waiting", 2000)
			time.Sleep(3500 * time.Millisecond)
			select {
			case c := <-v.calls:
				t.Fatalf("the tool ran with no final to ask: %s", c.Args)
			default:
			}
			if ends {
				r.sup.Stop("the exchange ends")
			}
			const fresh = "is ABC-12 merged?"
			r.hearNow(fresh, false)
			q := toolWithin(t, v, 2*time.Second)
			if ends {
				if q != "" {
					t.Errorf("a final after the exchange ended reached the tool: %q", q)
				}
				return
			}
			if q != fresh {
				t.Errorf("the awaited final reached the tool as %q, want %q", q, fresh)
			}
		})
	}
}

// TestIdenticalTextInDistinctTurnsKeepsBothOwners (the design note's proposed
// companion, section 7.3): a request is owned by its final, not by its words.
// The same question said again, in a new turn or as a new final of the same
// turn, is a new request and is asked, whole, once: a later delegation with
// no new final asks nothing, however the words before it read. At 8c192ba a
// repeat was asked only because the old rule replayed any delegation's last
// words, so the retry after it asked them a third time.
func TestIdenticalTextInDistinctTurnsKeepsBothOwners(t *testing.T) {
	for _, newTurn := range []bool{true, false} {
		name := "a new turn"
		if !newTurn {
			name = "a new final of the same turn"
		}
		t.Run(name, func(t *testing.T) {
			r, v, w := openVoiceB(t)
			const question = "what about ABC-42?"
			if q, _ := delegate(t, w, v, r, "first", 1000); q != question {
				t.Fatalf("first question %q", q)
			}
			if newTurn {
				r.hearNow("okay", true)
			}
			r.hearNow(question, newTurn)
			if q, _ := delegate(t, w, v, r, "again", 2000); q != question {
				t.Errorf("the repeated question reached the tool as %q, want %q", q, question)
			}
			sendDelegation(w, "retry", 3000)
			if q := toolWithin(t, v, 800*time.Millisecond); q != "" {
				t.Errorf("a delegation with no new final asked %q: the words, not the final, owned the request", q)
			}
		})
	}
}

// TestTwoDelegationsWaitingOnOneFinalAskItOnce (the design note, sections
// 4.2 and 4.6): a repeated delegation is a vendor event, not a new request.
// Two delegations wait for the same final; the first to claim it commits it
// (Exchange.Claim), and the other finds nothing left and closes silently, so
// the room's question reaches the tool once.
func TestTwoDelegationsWaitingOnOneFinalAskItOnce(t *testing.T) {
	r, v, w := openVoiceB(t)
	if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
		t.Fatalf("initial question %q", q)
	}
	sendDelegation(w, "a", 2000)
	sendDelegation(w, "b", 2100)
	time.Sleep(200 * time.Millisecond)
	const fresh = "is ABC-12 merged?"
	r.hearNow(fresh, false)
	if q := toolWithin(t, v, 2*time.Second); q != fresh {
		t.Fatalf("the awaited final reached the tool as %q, want %q", q, fresh)
	}
	if q := toolWithin(t, v, 800*time.Millisecond); q != "" {
		t.Errorf("one final was asked twice; the second asked %q", q)
	}
}
