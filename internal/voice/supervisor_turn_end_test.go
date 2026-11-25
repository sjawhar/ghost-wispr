package voice

import (
	"testing"
)

// TestTheTurnsEndIsToldToTheReadAhead: the Concierge's read-ahead holds a key
// of one digit that ends the words so far ("what about INFRA-7"), since more
// words may make it speech, and reads it when the turn ends. It fails unless
// the Heard call of the result that ends the turn (Deepgram's speech_final)
// says so, and no earlier call does.
func TestTheTurnsEndIsToldToTheReadAhead(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	r.heard("Wispr, what about", false, false)
	r.waitSession()
	r.heard("Wispr, what about INFRA-7", false, false)
	r.heard("Wispr, what about INFRA-7", true, true)
	r.waitTurn()
	calls := c.heardCalls()
	if len(calls) == 0 {
		t.Fatal("the Concierge's read-ahead heard nothing")
	}
	for i, h := range calls {
		if last := i == len(calls)-1; h.x.TurnEnded != last {
			t.Errorf("Heard call %d of %d (%q) says the turn ended %v, want %v", i+1, len(calls), h.words, h.x.TurnEnded, last)
		}
	}
}
