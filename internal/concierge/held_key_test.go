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
// ABC-12. A vendor can answer before the unit's transcript ends the turn, so
// no read at the turn's end is made yet. It fails unless the answer reads the
// key the read-ahead holds for the turn, so it names the issue the room
// said. "What about ABC-12" is the control, read ahead as on main.
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
// turn before as the exchange's AskerTurn (#192). The answer that read the
// key the read-ahead held ended the hold: a next turn whose vendor wording
// names no key reads it not again. It fails if the hold outlives its turn.
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

// TestAnEarlierAskerTurnsKeyIsNotReadAgain (Sol's review of #194 at 96c42c9,
// R3): voices B and C can leave the turn before as AskerTurn. After an answer
// in which the vendor named its own key, or the room picked an offered issue,
// a next keyless question read the earlier AskerTurn's key, and could ask its
// holder: 96c42c9 read the room's keys whenever no read of them existed. It
// fails unless the next question reads none of them and asks no one, as main
// does.
func TestAnEarlierAskerTurnsKeyIsNotReadAgain(t *testing.T) {
	for _, unit := range []string{"what about INFRA-7", "what about ABC-12 and INFRA-7"} {
		t.Run("the vendor's key won: "+unit, func(t *testing.T) {
			r := heldKeyRig(t, unit)
			r.concierge.Heard(context.Background(), &r.exchange, unit)
			time.Sleep(150 * time.Millisecond)
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is todo."))}, {chunk(text("Nothing else."))}}
			var first, second []said
			_ = r.concierge.Answer(context.Background(), &r.exchange, "what is the status of ABC-12", recorder(&first))
			before := r.records.calls()
			if r.exchange.IssueKey != "ABC-12" {
				t.Fatalf("control: the vendor's ABC-12 was not read: %q", before)
			}
			r.exchange.FollowUpTurn = true
			_ = r.concierge.Answer(context.Background(), &r.exchange, "and what about the schedule", recorder(&second))
			if again := r.records.calls()[len(before):]; slices.Contains(again, "INFRA-7") || slices.Contains(again, "ABC-12") {
				t.Fatalf("the next keyless question read %q, an earlier AskerTurn's", again)
			}
		})
	}
	t.Run("a pick", func(t *testing.T) {
		r := heldKeyRig(t, "ABC-12")
		r.concierge.offerPick(&r.exchange, "what is the status of the realtime voice", []candidate{{key: "ABC-12", title: "Realtime voice"}, {key: "INFRA-7", title: "Budget"}})
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is todo."))}, {chunk(text("Nothing else."))}}
		var first, second []said
		_ = r.concierge.Answer(context.Background(), &r.exchange, "ABC-12", recorder(&first))
		before := r.records.calls()
		if r.exchange.IssueKey != "ABC-12" {
			t.Fatalf("control: the offered ABC-12 was not read: %q", before)
		}
		r.exchange.FollowUpTurn = true
		_ = r.concierge.Answer(context.Background(), &r.exchange, "and what about the schedule", recorder(&second))
		if again := r.records.calls()[len(before):]; slices.Contains(again, "ABC-12") {
			t.Fatalf("the next keyless question read %q again", again)
		}
	})
	t.Run("no ask for the turn before's holder", func(t *testing.T) {
		r := heldKeyRig(t, "what about INFRA-7")
		issue := claimed("INFRA-7", "session", "agent-7", "Budget worker")
		issue.HolderState, issue.UpdatedAt = "live", testNow
		r.records.issues["INFRA-7"] = issue
		r.concierge.Heard(context.Background(), &r.exchange, r.exchange.AskerTurn)
		time.Sleep(100 * time.Millisecond)
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is todo."))}, {chunk(thought("No further facts."))}}
		var first, second []said
		_ = r.concierge.Answer(context.Background(), &r.exchange, "what is the status of ABC-12", recorder(&first))
		r.exchange.FollowUpTurn = true
		_ = r.concierge.Answer(context.Background(), &r.exchange, "ask the agent whether the schedule is ready", recorder(&second))
		if sent := r.agents.asked(); sent != 0 {
			t.Fatalf("sent %d questions for the turn before's INFRA-7 (spoken %+v)", sent, second)
		}
	})
}

// TestAHeldKeyIsReadWhenTheTurnEnds: the read-ahead holds a key of one digit
// that ends the words so far, since more words may make it speech, and reads
// it once the turn ends (Exchange.TurnEnded, Deepgram's endpoint), as main
// read a longer key while it was said: "what about INFRA-7" said alone reads
// INFRA-7 before the answer. "ops 1 and 2" reads nothing when its turn ends,
// and the hold "ops 1" had is gone with the words after it.
func TestAHeldKeyIsReadWhenTheTurnEnds(t *testing.T) {
	r := heldKeyRig(t, "")
	say := func(words string, ended bool) {
		x := r.exchange
		x.AskerTurn, x.TurnEnded = words, ended
		r.concierge.Heard(context.Background(), &x, words)
	}
	say("what about", false)
	say("what about INFRA-7", false)
	time.Sleep(100 * time.Millisecond)
	if calls := r.records.calls(); slices.Contains(calls, "INFRA-7") {
		t.Fatalf("INFRA-7 read %q before the turn ended", calls)
	}
	say("what about INFRA-7", true)
	eventually(t, "the read of INFRA-7 at the turn's end", func() bool { return slices.Contains(r.records.calls(), "INFRA-7") })

	r = speechRig(t)
	x := r.exchange
	for _, words := range []string{"ops", "ops 1", "ops 1 and", "ops 1 and 2"} {
		x.AskerTurn, x.TurnEnded = words, words == "ops 1 and 2"
		r.concierge.Heard(context.Background(), &x, words)
		time.Sleep(20 * time.Millisecond)
	}
	r.exchange.AskerTurn = "ops 1 and 2"
	var spoken []said
	_ = r.concierge.Answer(context.Background(), &r.exchange, "what about the ops work", recorder(&spoken))
	if calls := r.records.calls(); len(calls) != 0 || r.agents.asked() != 0 {
		t.Fatalf("\"ops 1 and 2\" read %q and sent %d questions; want none", calls, r.agents.asked())
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
