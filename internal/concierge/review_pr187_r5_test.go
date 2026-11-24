package concierge

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// Sol's fifth review of #187 at 0ba1d084
// (https://github.com/sjawhar/ghost-wispr/pull/187#pullrequestreview-6038129192).

// TestReview187R5FramedSelections (the R): the words people put around a
// choice frame a selection and add nothing to it; any other word still makes
// the reply talk, and a question's verb still makes it a question. Round 6
// narrowed the list: "let's do", "go for" and "I'd say" are new questions.
func TestReview187R5FramedSelections(t *testing.T) {
	for _, tc := range []struct {
		reply, key string
		ok         bool
	}{
		// Sol's two.
		{"I think the first one", "ABC-12", true},
		{"the first one then", "ABC-12", true},
		// Main's four, two of them new questions since round 6.
		{"let's do the first one", "", false},
		{"I guess the wake word one", "ABC-13", true},
		{"go for the first one", "", false},
		{"probably the second one", "ABC-13", true},
		// The rest of the list.
		{"I'd say the wake word one", "", false},
		{"maybe the first one", "ABC-12", true},
		{"actually, the second one", "ABC-13", true},
		{"just the first one", "ABC-12", true},
		{"the second one now", "ABC-13", true},
		{"lets do ABC-13", "", false},
		// Still talk: words outside the list.
		{"first coffee, then lunch", "", false},
		{lessonsReply, "", false},
		{"let's do lunch first", "", false},
		{"I think the first coffee is better", "", false},
		{"maybe the second item on the budget", "", false},
		// Still a question: a question's leading verb, or a question word.
		{"do you mean the first one", "", false},
		{"does the first one have a PR", "", false},
		{"I think what about the first one", "", false},
	} {
		r := newRig(t)
		r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
		_, key, ok := r.concierge.takePick(&r.exchange, tc.reply)
		if key != tc.key || ok != tc.ok {
			t.Errorf("takePick(%q) = %q, %v; want %q, %v", tc.reply, key, ok, tc.key, tc.ok)
		}
	}
}

// TestReview187R5FramedSelectionsSendThePendingQuestion: through Answer on a
// follow-up turn, Sol's two framed selections send the question first
// asked, once.
func TestReview187R5FramedSelectionsSendThePendingQuestion(t *testing.T) {
	for _, reply := range []string{"I think the first one", "the first one then"} {
		t.Run(reply, func(t *testing.T) {
			if got := pendingSends(t, twoOffered, reply); got != 1 {
				t.Errorf("sent the question first asked %d times, want 1", got)
			}
		})
	}
}

// TestReview187R5SpelledKeysNameKnownProjects (the I): letters said one by
// one read as an issue key only when they name a project Dispatch knows; any
// other letters stay as said.
func TestReview187R5SpelledKeysNameKnownProjects(t *testing.T) {
	r := newRig(t) // Dispatch knows ABC, XYZ, DEF and INFRA.
	for in, want := range map[string]string{
		"A B C thirteen":                 "ABC-13",
		"A. B. C. 12":                    "ABC-12",
		"what about D E F forty-one":     "what about DEF-41",
		"I O U twenty dollars":           "I O U twenty dollars",
		"A R S T fifteen hundred":        "A R S T fifteen hundred",
		"X V six evals":                  "X V six evals",
		"what's happening with A B C 13": "what's happening with ABC-13",
	} {
		if got := readKeys(t, r, in); got != want {
			t.Errorf("readSpelledKeys(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"what's happening with A B C thirteen": "keys:ABC-13|",
		"I O U twenty dollars":                 "search:twenty dollars",
		"what's happening with X V six evals?": "search:xv6 evals",
	} {
		if got := r.concierge.readTarget(readKeys(t, r, in)); got != want {
			t.Errorf("readTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReview187R5SpelledKeysThroughAnswer: through Answer on a follow-up
// turn, "A B C twelve" picks the offered ABC-12 and sends the question
// first asked; "I O U twenty dollars" is talk and sends nothing.
func TestReview187R5SpelledKeysThroughAnswer(t *testing.T) {
	one := []candidate{{"ABC-12", "Realtime voice"}}
	for reply, want := range map[string]int{"A B C twelve": 1, "I O U twenty dollars": 0} {
		t.Run(reply, func(t *testing.T) {
			if got := pendingSends(t, one, reply); got != want {
				t.Errorf("sent the question first asked %d times, want %d", got, want)
			}
		})
	}
}

// pendingSends offers candidates for twoOfferedTurn, answers reply on a
// follow-up turn, and counts the messages that carry the question first
// asked.
func pendingSends(t *testing.T, candidates []candidate, reply string) int {
	t.Helper()
	r := newRig(t)
	heldIssue(r, "session", false)
	r.exchange.FollowUpTurn = true
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
	r.concierge.offerPick(&r.exchange, twoOfferedTurn, candidates)
	_ = answerTurn(t, r, reply)
	r.agents.mu.Lock()
	sent := slices.Clone(r.agents.sent)
	r.agents.mu.Unlock()
	pending := 0
	for _, message := range sent {
		if strings.Contains(message.Message, "Is the tap merged?") {
			pending++
		}
	}
	return pending
}
