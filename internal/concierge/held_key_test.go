package concierge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
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
// R2): voices B and C answer in their vendor's words, which can leave the
// room's keys out: "what about ABC-12 and INFRA-7" said, "what about the
// realtime voice" answered. At 8998810 the read-ahead held INFRA-7 for the
// turn's end, the answer found no read, and the room heard "I found no issue
// for that". The read-ahead now reads the words so far as main's does, so
// the answer finds the read of the room's keys (AskerTurn, #192) and names
// the issue the room said. "What about ABC-12" is the control, a key of two
// digits, read ahead as on main.
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

// TestAOneDigitKeyIsReadAheadAsItIsSaid: the read-ahead reads a key of one
// digit in the words so far, as main's reads one of two digits, so the
// answer finds its read. "What about INFRA-7" reads INFRA-7 before the
// answer; "what about ABC-12 and INFRA-7" reads both. It fails if the
// read-ahead does not read the key while it is said.
func TestAOneDigitKeyIsReadAheadAsItIsSaid(t *testing.T) {
	for unit, want := range map[string][]string{
		"what about INFRA-7":            {"INFRA-7"},
		"what about infra 7":            {"INFRA-7"},
		"what about ABC-12 and INFRA-7": {"INFRA-7"},
	} {
		r := heldKeyRig(t, unit)
		r.concierge.Heard(context.Background(), &r.exchange, unit)
		for _, key := range want {
			eventually(t, "the read ahead of "+key+" for "+unit, func() bool { return slices.Contains(r.records.calls(), key) })
		}
	}
}

// TestAnAnswersOwnKeyIsNotReadForTheNextTurn: voices B and C can leave the
// turn before as the exchange's AskerTurn (#192). The answer that took the
// read ahead of INFRA-7 spent it: a next turn whose vendor wording names no
// key reads it not again. It fails if the read outlives its answer.
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

// TestAReadOfAKeyThatBecameSpeechReachesNoAnswer: while "ops 1" is the whole
// turn it is a bare reference, so the read-ahead reads OPS-1, as main's
// read-ahead reads any key in the words so far; "and 2" makes it speech.
// That read says and sends nothing, and no answer uses it: not the answer
// to "ops 1 and 2", in the unit's words or a vendor's, and not the next
// turn's, whose AskerTurn may still be "ops 1 and 2", or even "ops 1". It
// fails if OPS-1 reaches an answer's record, its words, its issue key or a
// send. The read ahead of OPS-1 is its control: the probe can see it.
func TestAReadOfAKeyThatBecameSpeechReachesNoAnswer(t *testing.T) {
	for _, tc := range []struct{ name, answered string }{
		{"the unit's words", "ops 1 and 2"},
		{"a vendor's words", "what about the ops work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := speechRig(t)
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Nothing names it."))}, {chunk(text("Nothing names it."))}, {chunk(text("Nothing names it."))}}
			heardTurn(r, "ops 1 and 2")
			if calls := r.records.calls(); !slices.Contains(calls, "OPS-1") {
				t.Fatalf("control: the read-ahead read %q, want OPS-1 while \"ops 1\" was the turn", calls)
			}
			ahead := len(r.records.calls())
			noOPS1 := func(stage string, spoken []said, err error) {
				t.Helper()
				if calls := r.records.calls()[ahead:]; slices.Contains(calls, "OPS-1") || r.exchange.IssueKey != "" || r.agents.asked() != 0 {
					t.Fatalf("%s: read %q, named %q, sent %d (Answer = %v, spoken %+v); want no OPS-1", stage, calls, r.exchange.IssueKey, r.agents.asked(), err, spoken)
				}
				for _, s := range spoken {
					if strings.Contains(s.text, "OPS-1") {
						t.Fatalf("%s said %q", stage, s.text)
					}
				}
				for _, request := range r.model.requests {
					if strings.Contains(promptOf(request), "OPS-1") {
						t.Fatalf("%s: the model's record names OPS-1", stage)
					}
				}
			}
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, tc.answered, recorder(&spoken))
			noOPS1("the answer", spoken, err)
			for _, stale := range []string{"ops 1 and 2", transcript.WriteKeys("ops 1", speechProjects)} {
				r.exchange.AskerTurn, r.exchange.FollowUpTurn = stale, true
				spoken = nil
				err = r.concierge.Answer(context.Background(), &r.exchange, "and what about the schedule", recorder(&spoken))
				noOPS1("the next turn, AskerTurn "+stale, spoken, err)
			}
		})
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
