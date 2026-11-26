package concierge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
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

// TestAVendorsWordingReadsWhatMainReads (Sol's review of #194 at 8998810,
// R2, and Main's rule after round 5): voices B and C answer in their
// vendor's words, which can leave the room's keys out: "what about ABC-12
// and INFRA-7" said, "what about the realtime voice" answered. The
// read-ahead reads no key of one digit, so the answer takes the read ahead
// of the room's keys of two digits or more (AskerTurn, #192), as main does:
// ABC-12. It reads no one-digit key the vendor's words leave out, as main,
// which reads none. Keys of one digit take no place in the read-ahead's three
// (maxKeys): "what about ABC-12 and INFRA-1 and INFRA-2 and INFRA-3" reads
// ABC-12, and "... ABC-12 and ABC-13 and ABC-14 and INFRA-7" tries ABC-14 and
// ABC-13, then ABC-12, as main (Sol's review of #194 at 07cc6a5, round 6). It
// fails if a key of one digit is read, or if ABC-12 is not named where the
// room said it. At 8998810 the mixed turn heard "I found no issue for that";
// at e5cf4d6 it named INFRA-7; at 07cc6a5 the two rows of four keys asked for
// the key. Each row has a rig of its own, so the rows run in parallel.
func TestAVendorsWordingReadsWhatMainReads(t *testing.T) {
	t.Parallel()
	for unit, want := range map[string]string{
		"what about ABC-12":                                     "ABC-12",
		"what about ABC-12 and INFRA-7":                         "ABC-12",
		"what about INFRA-7":                                    "",
		"what about infra 7":                                    "",
		"what about ABC-12 and INFRA-1 and INFRA-2 and INFRA-3": "ABC-12",
		"what about ABC-12 and ABC-13 and ABC-14 and INFRA-7":   "ABC-12",
	} {
		t.Run(unit, func(t *testing.T) {
			t.Parallel()
			r := heldKeyRig(t, unit)
			r.concierge.Heard(context.Background(), &r.exchange, unit)
			time.Sleep(150 * time.Millisecond)
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is todo."))}}
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, "what about the realtime voice", recorder(&spoken))
			calls := r.records.calls()
			if slices.ContainsFunc(calls, oneDigit) || r.exchange.IssueKey != want {
				t.Fatalf("reads %q, IssueKey %q, spoken %+v, err %v; want IssueKey %q and no read of a key of one digit", calls, r.exchange.IssueKey, spoken, err, want)
			}
		})
	}
}

// oneDigit reports whether key's number has one digit.
func oneDigit(key string) bool {
	_, number, _ := strings.Cut(key, "-")
	return len(number) == 1
}

// TestTheReadAheadKeepsMainsKeysUnderItsLimit (Sol's review of #194 at
// 07cc6a5, round 6): the read-ahead reads main's keys, two digits or more,
// the last said first, at most maxKeys of them; keys of one digit are left
// out before the limit, so they take none of its places. It fails if a key of
// one digit takes a place a key of two digits main reads would have had.
func TestTheReadAheadKeepsMainsKeysUnderItsLimit(t *testing.T) {
	r := newRig(t)
	view := r.concierge.viewProjects()
	for words, want := range map[string]string{
		"what about ABC-12 and INFRA-1 and INFRA-2 and INFRA-3": "keys:ABC-12",
		"what about ABC-12 and ABC-13 and ABC-14 and INFRA-7":   "keys:ABC-14,ABC-13,ABC-12",
		"what about INFRA-1 and ABC-12 and INFRA-2 and ABC-13":  "keys:ABC-13,ABC-12",
		"what about ABC-11 and ABC-12 and ABC-13 and ABC-14":    "keys:ABC-14,ABC-13,ABC-12",
		"what about INFRA-1 and INFRA-2":                        "",
	} {
		if got := r.concierge.aheadTarget(view, words); keysOf(got) != keysOf(want) {
			t.Errorf("aheadTarget(%q) = %q, want the keys of %q", words, got, want)
		}
	}
}

// TestTheReadAheadReadsNoOneDigitKey (Main's rule after Sol's review of #194
// at e5cf4d6): every read ahead of a key of one digit, held, released at the
// turn's end, or cancelled and then read again for a late transcript, read an
// issue the room did not mean (Sol's R2 to R5). The read-ahead reads none:
// the turn's keys of two digits or more are read ahead alone, as main reads
// them, and the answer reads a key of one digit from the turn's final words.
// It fails if a key of one digit is read before the answer, if the read-ahead
// searches a turn whose key of one digit the answer reads, or if the answer
// to "what about INFRA-7" does not read it. "What about ABC-12" is the
// control: its key is read ahead, so the probe can see a read ahead. Each row
// has a rig of its own, so the rows run in parallel.
func TestTheReadAheadReadsNoOneDigitKey(t *testing.T) {
	t.Parallel()
	for unit, want := range map[string][]string{
		"what about ABC-12":             {"ABC-12"},
		"what about ABC-12 and INFRA-7": {"ABC-12"},
		"what about INFRA-7":            nil,
		"what about infra 7":            nil,
		"infra 7":                       nil,
		"ops 1":                         nil,
		"no, not ops 1":                 nil,
		"what is blocking INFRA-7's realtime voice": nil,
	} {
		t.Run(unit, func(t *testing.T) {
			t.Parallel()
			r := speechRig(t)
			r.concierge.searchSettle = 50 * time.Millisecond
			r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Title: "Realtime voice", Status: "todo", UpdatedAt: testNow}
			r.records.issues["INFRA-7"] = &dispatch.Issue{Key: "INFRA-7", Title: "Realtime voice", Status: "todo", UpdatedAt: testNow}
			words := transcript.WriteKeys(unit, speechProjects)
			r.exchange.AskerTurn = words
			r.concierge.Heard(context.Background(), &r.exchange, words)
			for _, key := range want {
				eventually(t, "the read ahead of "+key, func() bool { return slices.Contains(r.records.calls(), key) })
			}
			time.Sleep(4 * r.concierge.searchSettle)
			calls := r.records.calls()
			if slices.ContainsFunc(calls, func(key string) bool { _, number, _ := strings.Cut(key, "-"); return len(number) == 1 }) {
				t.Errorf("the read-ahead read %q, want no key of one digit", calls)
			}
			if searches := r.records.searches(); len(want) == 0 && len(searches) != 0 {
				t.Errorf("the read-ahead searched %q, want nothing: the answer reads the turn's key", searches)
			}
		})
	}
	r := speechRig(t)
	r.records.issues["INFRA-7"] = &dispatch.Issue{Key: "INFRA-7", Title: "Realtime voice", Status: "todo", UpdatedAt: testNow}
	r.exchange.AskerTurn = "what about INFRA-7"
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-7 is todo."))}}
	answerTurn(t, r, "what about INFRA-7")
	if calls := r.records.calls(); !slices.Contains(calls, "INFRA-7") || r.exchange.IssueKey != "INFRA-7" {
		t.Fatalf("the answer to %q read %q and named %q; want INFRA-7", "what about INFRA-7", calls, r.exchange.IssueKey)
	}
}

// TestAnAnswersOwnKeyIsNotReadForTheNextTurn: voices B and C can leave the
// turn before as the exchange's AskerTurn (#192). The answer to "what about
// INFRA-7" reads INFRA-7 itself; a next turn whose vendor wording names no
// key reads it not again, though its AskerTurn is still "what about
// INFRA-7". It fails if a key read for one answer is read for the next.
func TestAnAnswersOwnKeyIsNotReadForTheNextTurn(t *testing.T) {
	const unit = "what about INFRA-7"
	r := heldKeyRig(t, unit)
	r.concierge.Heard(context.Background(), &r.exchange, unit)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("INFRA-7 is todo."))},
		{chunk(text("Nothing else."))},
	}
	answerTurn(t, r, unit)
	reads := func() int {
		return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != "INFRA-7" }))
	}
	before := reads()
	if before == 0 {
		t.Fatalf("control: the first answer read no INFRA-7: %q", r.records.calls())
	}
	r.exchange.IssueKey = ""
	var spoken []said
	_ = r.concierge.Answer(context.Background(), &r.exchange, "and what about the budget", recorder(&spoken))
	if got := reads(); got != before || r.exchange.IssueKey == "INFRA-7" {
		t.Fatalf("the next turn read INFRA-7 %d more times and named %q (spoken %+v); want neither: the AskerTurn was the turn before", got-before, r.exchange.IssueKey, spoken)
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

// TestAOneDigitKeyThatBecameSpeechIsNeverRead (Sol's reviews of #194, R5):
// while "ops 1" is the whole turn it is a bare reference, and "and 2" makes
// it speech. No read of OPS-1 is made, ahead or by an answer: not for the
// answer to "ops 1 and 2", in the unit's words or a vendor's, and not for
// the next turn's keyless ask after a late transcript of "ops 1" (Sol's R5
// schedule: the read ahead of OPS-1 is stalled until "and 2" cancels it, and
// the late "ops 1" reaches the read-ahead after the answer). At e5cf4d6 the
// late transcript read OPS-1 again and the ask went to its holder. It fails
// if OPS-1 is read, named, said, put in the model's record or sent to.
func TestAOneDigitKeyThatBecameSpeechIsNeverRead(t *testing.T) {
	for _, tc := range []struct{ name, answered string }{
		{"the unit's words", "ops 1 and 2"},
		{"a vendor's words", "what about the ops work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := speechRig(t)
			r.records.block, r.records.blockKey = make(chan struct{}), "OPS-1"
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Nothing names it."))}, {chunk(text("Nothing names it."))}, {chunk(text("Nothing names it."))}}
			noOPS1 := func(stage string, spoken []said, err error) {
				t.Helper()
				if calls := r.records.calls(); slices.Contains(calls, "OPS-1") || r.exchange.IssueKey != "" || r.agents.asked() != 0 {
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
			heardTurn(r, "ops 1 and 2")
			noOPS1("the read-ahead", nil, nil)
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, tc.answered, recorder(&spoken))
			noOPS1("the answer", spoken, err)
			close(r.records.block)
			late := transcript.WriteKeys("ops 1", speechProjects)
			r.exchange.AskerTurn = late
			r.concierge.Heard(context.Background(), &r.exchange, late)
			time.Sleep(150 * time.Millisecond)
			spoken = nil
			err = r.concierge.Answer(context.Background(), &r.exchange, "ask the agent whether the schedule is ready", recorder(&spoken))
			noOPS1("the next turn's ask, after a late \"ops 1\"", spoken, err)
			for _, stale := range []string{"ops 1 and 2", late} {
				r.exchange.AskerTurn, r.exchange.FollowUpTurn = stale, true
				spoken = nil
				err = r.concierge.Answer(context.Background(), &r.exchange, "and what about the schedule", recorder(&spoken))
				noOPS1("the next turn, AskerTurn "+stale, spoken, err)
			}
		})
	}
}

// TestADeclinedOfferLeavesNoRead (Sol's review of #194 at e5cf4d6, R6): "no,
// not OPS-1" declines an offered issue, and the answer that declined it
// returned before it took the read-ahead's reads. At e5cf4d6 the read-ahead
// had read OPS-1 while the room said it, and the next keyless question ("ask
// the agent whether the schedule is ready"), whose AskerTurn was still "no,
// not OPS-1", took that read and asked OPS-1's holder. With no key of one
// digit read ahead, that read is gone; with a key of two digits, main reads
// "no, not ABC-12" ahead and asks ABC-12's holder the same way. A declined
// offer's reads are dropped. It fails if the next question names the
// declined issue or sends to its holder.
func TestADeclinedOfferLeavesNoRead(t *testing.T) {
	for _, tc := range []struct {
		declined string
		offered  []candidate
	}{
		{"no, not ops 1", []candidate{{"OPS-1", "Recorder"}, {"ABC-12", "Recorder light"}}},
		{"no, not ABC-12", []candidate{{"ABC-12", "Recorder light"}, {"ABC-13", "Recorder button"}}},
	} {
		t.Run(tc.declined, func(t *testing.T) {
			r := speechRig(t)
			// No search finds an issue: only a read of the declined key could.
			r.records.hits, r.records.kwHits = nil, nil
			for _, key := range []string{"ABC-12", "ABC-13"} {
				session := "agent-" + strings.ToLower(key)
				r.records.issues[key] = &dispatch.Issue{Key: key, Title: "Recorder", Status: "in_progress", UpdatedAt: testNow, Claim: &dispatch.Claim{Actor: actor("session", session, "Worker")}}
				r.agents.states[session] = envoy.SessionInfo{SessionID: session, State: envoy.StateLive}
			}
			r.concierge.offerPick(&r.exchange, "what about the recorder", tc.offered)
			words := transcript.WriteKeys(tc.declined, speechProjects)
			r.exchange.AskerTurn = words
			r.concierge.Heard(context.Background(), &r.exchange, words)
			time.Sleep(150 * time.Millisecond)
			var spoken []said
			if err := r.concierge.Answer(context.Background(), &r.exchange, words, recorder(&spoken)); err != nil || len(spoken) == 0 || spoken[len(spoken)-1].text != "Okay." {
				t.Fatalf("control: the decline = %v, spoken %+v; want \"Okay.\"", err, spoken)
			}
			spoken = nil
			err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent whether the schedule is ready", recorder(&spoken))
			if r.exchange.IssueKey == tc.offered[0].key || r.agents.asked() != 0 {
				t.Fatalf("the next question named %q and sent %d questions (Answer = %v, spoken %+v); want neither the declined %s nor a send", r.exchange.IssueKey, r.agents.asked(), err, spoken, tc.offered[0].key)
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
