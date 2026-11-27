package concierge

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// Sol's third review of #187 at af8e612d
// (https://github.com/sjawhar/ghost-wispr/pull/187#pullrequestreview-6036570032).

// TestReview187R3OneOfferedIssueTakesEveryCue (finding 1): of one issue
// offered, a confirmation picks it by the same key, place and title cues as
// of two, and by a yes form or "that one" too; a cue that names something
// else picks none.
func TestReview187R3OneOfferedIssueTakesEveryCue(t *testing.T) {
	for _, tc := range []struct {
		reply, key string
		ok         bool
	}{
		// Sol's four.
		{"the first", "ABC-13", true},
		{"the wake word one", "ABC-13", true},
		{"yeah the wake word one", "ABC-13", true},
		{"ABC-13, the wake word one", "ABC-13", true},
		// Sol's controls, one offered.
		{"that one", "ABC-13", true},
		{"yes", "ABC-13", true},
		{"Yeah, that one.", "ABC-13", true},
		{"the first one", "ABC-13", true},
		{"ABC-13", "ABC-13", true},
		// A cue that names something else.
		{"the second one", "", false},
		{"yes, the second one", "", false},
		{"the realtime voice one", "", false},
		{"ABC-13, the second one", "", false},
		{"ABC-12, the wake word one", "", false},
	} {
		r := newRig(t)
		r.concierge.offerPick(&r.exchange, twoOfferedTurn, []candidate{{"ABC-13", "Wake word"}})
		_, key, ok := r.concierge.takePick(&r.exchange, tc.reply)
		if key != tc.key || ok != tc.ok {
			t.Errorf("takePick(%q) = %q, %v; want %q, %v", tc.reply, key, ok, tc.key, tc.ok)
		}
	}
}

// TestReview187R3TitleCuesReadEveryWord (finding 2): a title cue reads every
// word of the reply and of each title, not the five a search takes. A reply
// that names words of both titles picks none; a word past the fifth that
// one title holds and the other does not picks it.
func TestReview187R3TitleCuesReadEveryWord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []candidate
		reply, key string
		ok         bool
	}{
		{
			name:       "Sol's conflict",
			candidates: []candidate{{"ABC-12", "Realtime voice conversation launch pipeline"}, {"ABC-13", "Wake word"}},
			reply:      "ABC-12, the realtime voice conversation launch pipeline and wake word one",
		},
		{
			name:       "the sixth word tells two titles apart",
			candidates: []candidate{{"ABC-12", "Realtime voice conversation launch pipeline alpha"}, {"ABC-13", "Realtime voice conversation launch pipeline beta"}},
			reply:      "the beta one", key: "ABC-13", ok: true,
		},
		{
			name:       "control: two titles, two offered, one named",
			candidates: []candidate{{"ABC-12", "Realtime voice conversation launch pipeline"}, {"ABC-13", "Wake word"}},
			reply:      "the realtime voice conversation launch pipeline one", key: "ABC-12", ok: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.concierge.offerPick(&r.exchange, twoOfferedTurn, tc.candidates)
			_, key, ok := r.concierge.takePick(&r.exchange, tc.reply)
			if key != tc.key || ok != tc.ok {
				t.Errorf("takePick(%q) = %q, %v; want %q, %v", tc.reply, key, ok, tc.key, tc.ok)
			}
		})
	}
}

// TestReview187R3AConflictSendsNoPendingQuestion (finding 2): through
// Answer, on a follow-up turn, Sol's conflicting reply never sends the
// question first asked.
func TestReview187R3AConflictSendsNoPendingQuestion(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.exchange.FollowUpTurn = true
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
	r.concierge.offerPick(&r.exchange, twoOfferedTurn, []candidate{{"ABC-12", "Realtime voice conversation launch pipeline"}, {"ABC-13", "Wake word"}})
	_ = replyTurn(t, r, "ABC-12, the realtime voice conversation launch pipeline and wake word one")
	r.agents.mu.Lock()
	defer r.agents.mu.Unlock()
	for _, sent := range r.agents.sent {
		if strings.Contains(sent.Message, "tap") {
			t.Errorf("sent the question first asked: %q", sent.Message)
		}
	}
}
