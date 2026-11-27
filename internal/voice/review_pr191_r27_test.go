package voice

import (
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// TestAnOlderCallKeepsTheNewerQuestion: a call that Deepgram sends late,
// after the interim of a question said later, and that is another call than
// the exchange's (said at 2 s, where the exchange's call "Wispr." was at 5 s,
// so not the same call by sameCall), begins no new turn and resets nothing:
// the question stays pending, and its endpoint hands both, the older call's
// words first (Sol's twenty-seventh review of #191). Sol's TestSolR27LateRealCall
// sends the call's own final, which is the same call and so never reaches the
// reset; this one reaches it.
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
	r.s.mu.Unlock()
	if pending != "check the budget Can you send these notes?" || asker != pending || newTurn {
		t.Fatalf("the older call reset the newer turn: turn=%q AskerTurn=%q new turn=%v", pending, asker, newTurn)
	}
	r.s.OnUtteranceEnd(0, true, q.endsAt(), monoclock.Now())
	if got := r.waitTurn(); !strings.HasSuffix(got, "Can you send these notes?") {
		t.Fatalf("the question's endpoint handed %q", got)
	}
}
