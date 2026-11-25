package concierge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// heldKeyRig is a rig with ABC-12 and INFRA-7, no search hit, and the unit's
// own transcript of the turn, unit, as the exchange's AskerTurn: voices B and
// C answer in their vendor's words, while the read-ahead reads the unit's.
func heldKeyRig(t *testing.T, unit string) *rig {
	t.Helper()
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Title: "Realtime voice", Status: "todo", UpdatedAt: testNow}
	r.records.issues["INFRA-7"] = &dispatch.Issue{Key: "INFRA-7", Title: "Realtime voice", Status: "todo", UpdatedAt: testNow}
	r.exchange.AskerTurn = unit
	return r
}

// TestAHeldKeyIsReadForTheVendorsWording (Sol's review of #194 at 8998810,
// R2): the read-ahead holds a key of one digit that ends the words so far,
// and a vendor's wording that leaves the room's keys out found no read of
// them: "what about ABC-12 and INFRA-7" said, "what about the realtime
// voice" answered, and the room heard "I found no issue for that". Main read
// ABC-12. It fails unless the answer reads the room's keys itself when no
// read of them exists, so it names the issue the room said. "What about
// ABC-12" is the control, read ahead as on main.
func TestAHeldKeyIsReadForTheVendorsWording(t *testing.T) {
	for unit, want := range map[string]string{
		"what about ABC-12":             "ABC-12",
		"what about ABC-12 and INFRA-7": "INFRA-7",
		"what about INFRA-7":            "INFRA-7",
		"what about infra 7":            "INFRA-7",
	} {
		t.Run(unit, func(t *testing.T) {
			r := heldKeyRig(t, unit)
			r.concierge.Heard(context.Background(), &r.exchange, unit)
			time.Sleep(150 * time.Millisecond)
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text(want + " is todo."))}}
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, "what about the realtime voice", recorder(&spoken))
			if calls := r.records.calls(); !slices.Contains(calls, want) || r.exchange.IssueKey != want {
				t.Fatalf("reads %q, IssueKey %q, spoken %+v, err %v; want %s read and named", calls, r.exchange.IssueKey, spoken, err, want)
			}
		})
	}
}

// TestTheHoldHoldsOnlyTheLastKey (Sol's review of #194, R2): the read-ahead
// holds only the key of one digit that ends the words so far, which more
// words may make speech; the keys before it are read ahead. It fails if
// "what about ABC-12 and INFRA-7" reads no key ahead, or reads INFRA-7
// before the turn ends.
func TestTheHoldHoldsOnlyTheLastKey(t *testing.T) {
	r := heldKeyRig(t, "")
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 and INFRA-7")
	eventually(t, "the read ahead of ABC-12", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
	time.Sleep(100 * time.Millisecond)
	if calls := r.records.calls(); slices.Contains(calls, "INFRA-7") {
		t.Fatalf("the read-ahead read %q; want INFRA-7 held until the turn ends", calls)
	}
}

// TestAnAnswersOwnKeyIsNotReadForTheNextTurn: voices B and C can leave the
// turn before as the exchange's AskerTurn (#192). The room's keys the answer
// read itself, with no read ahead, are spent with it, as a read ahead is: a
// next turn whose vendor wording names no key reads none of them again. It
// fails if reading the room's keys afresh reads the turn before's.
func TestAnAnswersOwnKeyIsNotReadForTheNextTurn(t *testing.T) {
	const unit = "what about infra 7"
	r := heldKeyRig(t, unit)
	r.concierge.Heard(context.Background(), &r.exchange, unit)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("INFRA-7 is todo."))},
		{chunk(text("Nothing else."))},
	}
	answerTurn(t, r, "what about the realtime voice")
	reads := func() int {
		return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != "INFRA-7" }))
	}
	before := reads()
	if before == 0 {
		t.Fatalf("control: the first answer read no INFRA-7: %q", r.records.calls())
	}
	var spoken []said
	_ = r.concierge.Answer(context.Background(), &r.exchange, "and what about the budget", recorder(&spoken))
	if got := reads(); got != before {
		t.Fatalf("the next turn read INFRA-7 %d more times (spoken %+v); want none: the AskerTurn was the turn before", got-before, spoken)
	}
}

// TestAOneDigitKeysPossessiveIsNoSearchWord (Sol's review of #194, I1): "what
// is blocking INFRA-7's realtime voice" sent the full search "what is
// blocking s realtime voice": the key went, its "'s" stayed. It fails unless
// the possessive goes with the key of one digit, as with one of two digits.
func TestAOneDigitKeysPossessiveIsNoSearchWord(t *testing.T) {
	r := newRig(t)
	view := r.concierge.viewProjects()
	for turn, want := range map[string]string{
		"what is blocking INFRA-7's realtime voice":  "what is blocking realtime voice",
		"what is blocking INFRA-7’s realtime voice":  "what is blocking realtime voice",
		"what is blocking ABC-12's realtime voice":   "what is blocking realtime voice",
		"what is blocking INFRA-7 on realtime voice": "what is blocking on realtime voice",
	} {
		if question, _ := searchOf(r.concierge.readTarget(view, turn)); question != want {
			t.Errorf("readTarget(%q) searches %q, want %q", turn, question, want)
		}
	}
	if !strings.Contains(r.concierge.readTarget(view, "what is blocking INFRA-7's realtime voice"), "INFRA-7") {
		t.Errorf("control: INFRA-7's is no key")
	}
}
