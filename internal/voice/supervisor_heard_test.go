package voice

import (
	"testing"
)

// TestTheConciergeHearsTheTurnAsItIsSpoken fails if the Concierge is not told
// the asker's turn so far on each result after the wake, or is told it for an
// exchange keyed differently (session id, wake time) from the one it later
// answers: its read of the record while the asker speaks would go unused.
func TestTheConciergeHearsTheTurnAsItIsSpoken(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	r.heard("Wispr, what is happening with", false, false)
	r.waitSession()
	r.heard("Wispr, what is happening with the voice issue?", true, true)
	r.waitTurn()

	heard := c.heardCalls()
	if len(heard) < 2 {
		t.Fatalf("the Concierge heard the turn %d times, want once per result", len(heard))
	}
	if got := heard[0].words; got != "what is happening with" {
		t.Errorf("first heard %q, want the interim words after the wake phrase", got)
	}
	if got := heard[len(heard)-1].words; got != "what is happening with the voice issue?" {
		t.Errorf("last heard %q, want the final turn", got)
	}
	answered := c.exchanges[0]
	for i, h := range heard {
		if h.x.SessionID != answered.SessionID || !h.x.WakeAt.Equal(answered.WakeAt) {
			t.Errorf("heard %d keyed (%q, %v), answered (%q, %v): the keys differ", i, h.x.SessionID, h.x.WakeAt, answered.SessionID, answered.WakeAt)
		}
	}
}

// TestTheTurnTakesTheSessionOnceItStarts fails if an exchange woken before the
// recording session started keeps an empty session id after it starts: the
// Concierge's questions and reads need it.
func TestTheTurnTakesTheSessionOnceItStarts(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	r.setSession("") // no session yet
	r.heard("Wispr,", false, false)
	sess := r.waitSession()
	if sess.x.SessionID != "" {
		t.Fatalf("session %q at open, want none yet", sess.x.SessionID)
	}
	r.setSession("sess-2") // the session starts, the one after the exchange opened
	r.heard("Wispr, what is happening?", true, true)
	r.waitTurn()
	heard := c.heardCalls()
	if last := heard[len(heard)-1]; last.x.SessionID != "sess-2" {
		t.Errorf("heard keyed on session %q, want sess-2", last.x.SessionID)
	}
	if got := c.exchanges[0].SessionID; got != "sess-2" {
		t.Errorf("answered for session %q, want sess-2", got)
	}
}
