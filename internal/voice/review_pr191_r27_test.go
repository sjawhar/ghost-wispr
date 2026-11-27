package voice

import (
	"slices"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// TestAnOlderCallKeepsTheNewerQuestion: a call that Deepgram sends late,
// after the interim of a question said later, and that is another call than
// the exchange's (said at 2 s, where the exchange's call "Wispr." was at 5 s,
// so not the same call by sameCall), begins no new turn and resets nothing:
// the question stays pending, alone, and its endpoint hands it; the older
// call's words are the room's history only (Sol's twenty-seventh and
// twenty-eighth reviews of #191). Sol's TestSolR27LateRealCall sends the
// exchange's own call's final, which never reaches the reset; this one does.
func TestAnOlderCallKeepsTheNewerQuestion(t *testing.T) {
	r, _, _, captured := r27Open(t) // the office hold, then the call "Wispr." at 5 s
	q := r27Hear(r, captured, "Can you send these notes?", 10, false, false, 4)
	r.s.mu.Lock()
	ex := r.s.ex
	turn := ex.x.Turn
	r.s.mu.Unlock()
	late := r27Hear(r, captured, "Wispr, check the budget", 2, true, true, 9)
	if !late.endsAt().Before(q.Words[0].At) {
		t.Fatal("control: the late call does not end before the question began")
	}
	r.s.mu.Lock()
	pending, asker, newTurn := r.s.turnOf(ex), ex.x.AskerTurn, ex.x.Turn != turn
	var history []string
	for _, line := range r.s.recentLocked() {
		history = append(history, line.Text)
	}
	r.s.mu.Unlock()
	if pending != "Can you send these notes?" || asker != pending || newTurn {
		t.Fatalf("the older call changed the newer turn: turn=%q AskerTurn=%q new turn=%v", pending, asker, newTurn)
	}
	if !slices.Contains(history, "Wispr, check the budget") {
		t.Fatalf("the older call is not in the room's history: %q", history)
	}
	r.s.OnUtteranceEnd(0, true, q.endsAt(), monoclock.Now())
	if got := r.waitTurn(); got != "Can you send these notes?" {
		t.Fatalf("the question's endpoint handed %q", got)
	}
}
