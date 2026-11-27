package voice

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// Sol's twenty-fifth review of #191 at 053303b
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6086871370):
// during a misheard-name hold an UtteranceEnd, or a turn timer already
// queued, handed the words of an interim that the final withdrew to Answer.
// Sol's two probes (review_pr191_r25_sol_{endpoint,expiry}_test.go) need
// every part of the fix broken to fail; each test here holds one part.

// r25WithdrawnHold opens the office hold, ends it with a second call heard as
// an interim, and begins a second hold with the final that withdraws it. The
// results carry capture times, as Deepgram's do: the call's interim at 5 s,
// and its final from the same start, shorter than it ("Whisper. No. Wait,"
// ends before "notes" began), so only the shared start says that the final
// replaces it (Sol's twenty-seventh review of #191).
func r25WithdrawnHold(t *testing.T) (*rig, *fakeConcierge, *fakeSession, *exchange) {
	t.Helper()
	c := newFakeConcierge()
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
	captured := time.Now().Add(-time.Minute)
	r27Hear(r, captured, "Whisper", 0, false, false, 3)
	sess := r.waitSession()
	r27Hear(r, captured, "Whisper. No. Oh, yeah,", 0, true, true, 3)
	r.s.mu.Lock()
	ex := r.s.ex
	r.s.mu.Unlock()
	interim := r27Hear(r, captured, "Wispr, send these notes", 5, false, false, 4)
	r.s.mu.Lock()
	before := r.s.turnOf(ex)
	r.s.mu.Unlock()
	if before != "send these notes" {
		t.Fatalf("control: the second call's interim is not the turn: %q", before)
	}
	final := r27Hear(r, captured, "Whisper. No. Wait,", 5, true, true, 4)
	if !final.endsAt().Before(interim.Words[len(interim.Words)-1].At) {
		t.Fatal("control: the final does not end before the interim's last word began")
	}
	r.s.mu.Lock()
	held := ex.misheardHold && r.s.ex == ex
	r.s.mu.Unlock()
	if !held {
		t.Fatal("control: the withdrawing final began no hold")
	}
	return r, c, sess, ex
}

func r25NoAnswer(t *testing.T, c *fakeConcierge, what string) {
	t.Helper()
	select {
	case got := <-c.asked:
		t.Fatalf("%s handed %q to Answer", what, got)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestAWithdrawnInterimIsNoTurn: the final that withdraws an interim takes its
// words out of the turn, the exchange's record of it and the room's asker.
func TestAWithdrawnInterimIsNoTurn(t *testing.T) {
	r, _, sess, ex := r25WithdrawnHold(t)
	r.s.mu.Lock()
	turn, asker := r.s.turnOf(ex), ex.x.AskerTurn
	r.s.mu.Unlock()
	if turn != "" || asker != "" {
		t.Fatalf("the withdrawn interim is still the turn: turnOf %q, AskerTurn %q", turn, asker)
	}
	if room := sess.x.Room(); strings.Contains(room.Asker, "send these notes") {
		t.Fatalf("the room's asker still says the withdrawn words: %q", room.Asker)
	}
}

// TestTheHoldHandsNoTurnOver: handTurnLocked itself hands nothing over while
// the hold holds, whatever called it; once a call or a question ended the
// hold, the same turn is handed.
func TestTheHoldHandsNoTurnOver(t *testing.T) {
	for _, released := range []bool{false, true} {
		name := "held"
		if released {
			name = "control_released"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			officeWake(t, r)
			r.s.mu.Lock()
			ex := r.s.ex
			ex.interim = "send these notes" // words the hold holds, by hand
			if released {
				r.s.endHoldLocked(ex)
			}
			r.s.handTurnLocked(ex, turnSignal{kind: signalUtteranceEnd, at: monoclock.Now()})
			answering := ex.answering
			r.s.mu.Unlock()
			if released {
				if got := r.waitTurn(); got != "send these notes" {
					t.Fatalf("control: the released turn handed %q", got)
				}
				return
			}
			if answering {
				t.Fatal("handTurnLocked began answering during the hold")
			}
			r25NoAnswer(t, c, "handTurnLocked during the hold")
		})
	}
}

// TestATurnTimerDuringTheHoldEndsNothing: a turn timer already queued when the
// hold began neither answers nor ends the held exchange as "no question"; the
// hold's question gate still takes a question that comes within its bound.
func TestATurnTimerDuringTheHoldEndsNothing(t *testing.T) {
	r, c, sess, ex := r25WithdrawnHold(t)
	r.s.turnWaited(ex)
	if closedNow(sess) || !r24SameExchange(r, ex) {
		t.Fatal("a queued turn timer ended the held exchange")
	}
	r25NoAnswer(t, c, "a queued turn timer during the hold")
	r.heardAs("Can you send these notes to Raj?", true, true, 2)
	if got := r.waitTurn(); !strings.Contains(got, "send these notes to Raj") {
		t.Fatalf("the question within the hold handed %q", got)
	}
}
