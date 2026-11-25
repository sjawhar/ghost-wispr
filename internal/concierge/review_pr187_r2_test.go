package concierge

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// Sol's second review of #187 at fa049477
// (https://github.com/sjawhar/ghost-wispr/pull/187#pullrequestreview-6035936022).

// twoOffered is the question and the two issues Sol's probes offered.
const twoOfferedTurn = "ask the agent on the realtime voice whether the tap is merged"

var twoOffered = []candidate{{"ABC-12", "Realtime voice"}, {"ABC-13", "Wake word"}}

// TestReview187R2OneGateBeforeEveryPicker (finding 1): a reply picks an
// offered issue only when it is a confirmation: no question mark, no
// question word and no negation. A question or a negation is a new question,
// whatever key, place or title it names; a whole no picks none. A
// confirmation picks the issue every cue in it names, its key, its place or
// words of one title, and none when they disagree.
func TestReview187R2OneGateBeforeEveryPicker(t *testing.T) {
	for _, tc := range []struct {
		reply, key string
		ok         bool
	}{
		// Sol's three.
		{"What is blocking ABC-12, the realtime voice one?", "", false},
		{"No, not ABC-12, the realtime voice one.", "", false},
		{"Not ABC-12, the first one.", "", false},
		// Questions and negations with no question mark.
		{"what is blocking the realtime voice one", "", false},
		{"which one is the first one", "", false},
		{"does the first one have a PR", "", false},
		{"can you ask the wake word one", "", false},
		{"what's up with the first one", "", false},
		{"I don't mean the first one", "", false},
		{"never the realtime voice one", "", false},
		{"the second one isn't it", "", false},
		// The side talk of the earlier rounds.
		{"What is blocking ABC-12?", "", false},
		{"what is blocking ABC-12", "", false},
		{"Is that right?", "", false},
		{"is that right", "", false},
		{"That's 100, right?", "", false},
		{"yes and no", "", false},
		{"what about the tests?", "", false},
		{"the first item in our budget is rent", "", false},
		{"let us discuss the budget", "", false},
		// A whole no picks none.
		{"No.", "", true},
		{"Nope, not that one.", "", true},
		{"No, not ABC-13.", "", true},
		{"Neither.", "", true},
		// Confirmations by key, place and title.
		{"ABC 13", "ABC-13", true},
		{"ABC-13, please", "ABC-13", true},
		{"the first one", "ABC-12", true},
		{"The second one.", "ABC-13", true},
		{"the other one", "ABC-13", true},
		{"Yes, the first one.", "ABC-12", true},
		{"uh, the second one please", "ABC-13", true},
		{"the wake word one", "ABC-13", true},
		{"The realtime voice one, please.", "ABC-12", true},
		{"ABC-13, the wake word one.", "ABC-13", true},
		{"ABC-12, the first one", "ABC-12", true},
		// Cues that disagree pick none.
		{"ABC-12, the wake word one", "", false},
		{"the first one, the wake word one", "", false},
		{"ABC-14, the first one", "", false},
	} {
		r := newRig(t)
		r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
		_, key, ok := r.concierge.takePick(&r.exchange, tc.reply)
		if key != tc.key || ok != tc.ok {
			t.Errorf("takePick(%q) = %q, %v; want %q, %v", tc.reply, key, ok, tc.key, tc.ok)
		}
	}
}

// TestReview187R2AQuestionWithAPlaceSendsItsOwnWords (finding 1): through
// Answer, Sol's three replies never send the question first asked.
func TestReview187R2AQuestionWithAPlaceSendsItsOwnWords(t *testing.T) {
	for _, reply := range []string{
		"What is blocking ABC-12, the realtime voice one?",
		"No, not ABC-12, the realtime voice one.",
		"Not ABC-12, the first one.",
	} {
		t.Run(reply, func(t *testing.T) {
			r := newRig(t)
			heldIssue(r, "session", false)
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
			r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
			_ = replyTurn(t, r, reply)
			r.agents.mu.Lock()
			defer r.agents.mu.Unlock()
			for _, sent := range r.agents.sent {
				if strings.Contains(sent.Message, "tap") {
					t.Errorf("sent the question first asked: %q", sent.Message)
				}
			}
		})
	}
}

// TestReview187R2PluralPairs (finding 2): a title word and a searched word
// match when they are equal, or one is the other plus "s", plus "es", or,
// for a word that ends in a consonant and "y", with "ies" for the "y"; both
// ways. A stopword or a word under three letters matches nothing.
func TestReview187R2PluralPairs(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"policy", "policies", true},
		{"bus", "buses", true},
		{"alias", "aliases", true},
		{"cookie", "cookies", true},
		{"eval", "evals", true},
		{"box", "boxes", true},
		{"church", "churches", true},
		{"gas", "gases", true},
		{"key", "keys", true},
		{"use", "uses", true},
		{"us", "uses", false},
		{"key", "keies", false},
		{"cookie", "cooky", false},
		{"bus", "bust", false},
		{"xv6", "xv60", false},
	} {
		for _, pair := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			if got := titled("Fix the "+pair[0], pair[1]); got != tc.want {
				t.Errorf("titled(%q, %q) = %v, want %v", "Fix the "+pair[0], pair[1], got, tc.want)
			}
		}
	}
}

// TestReview187R2TheBusIssueIsKept (finding 2): Sol's probe. The keyword
// fallback keeps the issue whose title says "buses" for "bus".
func TestReview187R2TheBusIssueIsKept(t *testing.T) {
	r := newRig(t)
	r.concierge.fullWait = 40 * time.Millisecond
	r.records.searchDelay = time.Hour
	r.records.kwHits = []dispatch.SearchHit{
		{Kind: "issue", IssueKey: "ABC-12", Title: "Replace buses", Score: 0.5},
		{Kind: "issue", IssueKey: "ABC-13", Title: "Office lights", Score: 0.49},
	}
	if said := answerTurn(t, r, "what is happening with the bus?"); len(said) != 1 || said[0].text != "Did you mean A B C twelve?" {
		t.Errorf("spoken = %+v, want the buses issue", said)
	}
}
