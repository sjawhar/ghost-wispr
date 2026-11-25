package concierge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// Sol's fourth review of #187 at bee0abe8
// (https://github.com/sjawhar/ghost-wispr/pull/187#pullrequestreview-6037367522):
// a reply picks only when it is a selection, not when it merely holds a cue.

const lessonsReply = "okay so anyway lunch reservations, dinner plans, shopping, groceries, then voice lessons"

// TestReview187R4SideTalkIsNoSelection (the finding): a reply that holds a
// cue and other words of substance picks nothing, however many issues were
// offered.
func TestReview187R4SideTalkIsNoSelection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []candidate
		reply      string
	}{
		{"one offered, a place among other words", []candidate{{"ABC-13", "Wake word"}}, "first coffee, then lunch"},
		{"one offered, a title word among other words", []candidate{{"ABC-13", "Wake word"}}, "okay so anyway the word count reached 500"},
		{"one offered, a title word late in a long reply", []candidate{{"ABC-12", "Realtime voice"}}, lessonsReply},
		{"two offered, a title word late in a long reply", twoOffered, lessonsReply},
		{"two offered, a place among other words", twoOffered, "first coffee, then lunch"},
		{"two offered, a key among other words", twoOffered, "ABC-12 shipped last week, then lunch"},
		{"control: two offered, no cue", twoOffered, strings.Replace(lessonsReply, "voice", "piano", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.concierge.offerPick(&r.exchange, twoOfferedTurn, tc.candidates)
			if asked, key, ok := r.concierge.takePick(&r.exchange, tc.reply); key != "" || ok {
				t.Errorf("takePick(%q) = %q, %q, %v; want no pick", tc.reply, asked, key, ok)
			}
		})
	}
}

// TestReview187R4SideTalkSendsNoPendingQuestion: through Answer on a
// follow-up turn, side talk sends nothing; a selection sends the question
// first asked, once.
func TestReview187R4SideTalkSendsNoPendingQuestion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []candidate
		reply      string
		want       int
	}{
		{"one offered, a place", []candidate{{"ABC-12", "Realtime voice"}}, "first coffee, then lunch", 0},
		{"one offered, a title word", []candidate{{"ABC-12", "Realtime voice"}}, lessonsReply, 0},
		{"two offered, a title word", twoOffered, lessonsReply, 0},
		{"control: one offered, its key", []candidate{{"ABC-12", "Realtime voice"}}, "ABC-12, please", 1},
		{"control: two offered, a place", twoOffered, "the first", 1},
		{"control: two offered, a title", twoOffered, "yeah, the realtime voice one", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			heldIssue(r, "session", false)
			r.exchange.FollowUpTurn = true
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
			r.concierge.offerPick(&r.exchange, twoOfferedTurn, tc.candidates)
			_ = replyTurn(t, r, tc.reply)
			r.agents.mu.Lock()
			sent := slices.Clone(r.agents.sent)
			r.agents.mu.Unlock()
			pending := 0
			for _, message := range sent {
				if strings.Contains(message.Message, "Is the tap merged?") {
					pending++
				}
			}
			if pending != tc.want {
				t.Errorf("sent the question first asked %d times, want %d; sent %+v", pending, tc.want, sent)
			}
		})
	}
}

// TestReview187R4SideTalkThroughTheSupervisor: through the real Supervisor,
// after the one-issue confirmation of a bare number, side talk sends
// nothing and a yes sends the question.
func TestReview187R4SideTalkThroughTheSupervisor(t *testing.T) {
	for _, tc := range []struct {
		reply string
		want  int
	}{
		{"first coffee, then lunch", 0},
		{lessonsReply, 0},
		{strings.Replace(lessonsReply, "voice", "piano", 1), 0},
		{"yes", 1},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			r := newR7Rig(t)
			records := r.c.records.(*fakeRecords)
			records.mu.Lock()
			records.issues["ABC-4592"] = &dispatch.Issue{Key: "ABC-4592", Title: "Realtime voice", Status: "in_progress", UpdatedAt: time.Now(), Claim: &dispatch.Claim{Actor: actor("session", "agent-7", "Worker")}}
			records.mu.Unlock()
			r.hear("Wispr, ask him for 4592")
			r7Until(t, "confirmation spoken", func() bool {
				return slices.ContainsFunc(r.voice.said(), func(s r7Said) bool { return strings.HasPrefix(s.text, "Did you mean ") })
			})
			time.Sleep(20 * time.Millisecond)
			r.hear(tc.reply)
			r7Until(t, "follow-up completed", func() bool { o, c := r.voice.counts(); return o == 1 && c == 1 })
			r.agents.mu.Lock()
			sent := len(r.agents.sent)
			r.agents.mu.Unlock()
			if sent != tc.want {
				t.Errorf("sent %d question(s), want %d", sent, tc.want)
			}
		})
	}
}

// TestReview187R4SelectionMatrix: Sol's matrix of one offered issue, and
// the same selections of two.
func TestReview187R4SelectionMatrix(t *testing.T) {
	one := []candidate{{"ABC-13", "Wake word"}}
	for _, tc := range []struct {
		candidates []candidate
		reply, key string
		ok         bool
	}{
		{one, "first please", "ABC-13", true},
		{one, "the former", "ABC-13", true},
		{one, "1st", "ABC-13", true},
		{one, "wake word", "ABC-13", true},
		{one, "the wake one", "ABC-13", true},
		{one, "the word one", "ABC-13", true},
		{one, "yeah wake word", "ABC-13", true},
		{one, "ABC-13, first please", "ABC-13", true},
		{one, "yes ABC-13", "ABC-13", true},
		{one, "the first wake word one", "ABC-13", true},
		{one, "A B C thirteen", "ABC-13", true},
		{one, "uh, yeah the Wake Word one.", "ABC-13", true},
		{one, "that's the wake word one", "ABC-13", true},
		{one, "please use the wake word issue", "ABC-13", true},
		{one, "yes", "ABC-13", true},
		{one, "that one", "ABC-13", true},
		{one, "Yeah, that one.", "ABC-13", true},
		{one, "Sure, that's it.", "ABC-13", true},
		{one, "second please", "", false},
		{one, "the latter", "", false},
		{one, "the other", "", false},
		{one, "2nd", "", false},
		{one, "yes, the second wake word one", "", false},
		{one, "ABC-12", "", false},
		{one, "ABC-12, the wake word one", "", false},
		{one, "ABC-13, the second one", "", false},
		{one, "ABC-13 and ABC-12", "", false},
		{one, "the realtime voice one", "", false},
		{one, "what about wake word", "", false},
		{one, "the wake word one?", "", false},
		{one, "please don't pick the wake word one", "", false},
		{one, "never the first one", "", false},
		{one, "yes but", "", false},
		{one, "sure, whatever", "", false},
		{one, "okay so anyway", "", false},
		{one, "No.", "", true},
		{one, "No, not ABC-13.", "", true},
		{one, "neither one", "", true},
		{twoOffered, "first please", "ABC-12", true},
		{twoOffered, "the former", "ABC-12", true},
		{twoOffered, "2nd", "ABC-13", true},
		{twoOffered, "yeah wake word", "ABC-13", true},
		{twoOffered, "ABC-13, first please", "", false},
		{twoOffered, "A B C thirteen", "ABC-13", true},
		{twoOffered, "uh, yeah the Wake Word one.", "ABC-13", true},
		{twoOffered, "the real time voice one", "ABC-12", true},
		{twoOffered, "yes", "", false},
		{twoOffered, "that one", "", false},
	} {
		r := newRig(t)
		r.concierge.offerPick(&r.exchange, twoOfferedTurn, tc.candidates)
		// As answer reads a turn: a key said letter by letter first.
		_, key, ok := r.concierge.takePick(&r.exchange, readKeys(t, r, tc.reply))
		if key != tc.key || ok != tc.ok {
			t.Errorf("takePick(%q) of %d offered = %q, %v; want %q, %v", tc.reply, len(tc.candidates), key, ok, tc.key, tc.ok)
		}
	}
}

// TestReview187R4ASpelledProjectIsAKey (Sol's I): a key whose project is
// said letter by letter, as the voice now says one ("A B C thirteen"), is
// that key when Dispatch knows the project (round 5); spelled letters that
// make no key stay as they were.
func TestReview187R4ASpelledProjectIsAKey(t *testing.T) {
	r := newRig(t) // Dispatch knows ABC, XYZ, DEF and INFRA.
	for in, want := range map[string][]string{
		"A B C thirteen":                          {"ABC-13"},
		"A B C forty-five ninety-two":             {"ABC-4592"},
		"what changed on D E F fifteen hundred":   {"DEF-1500"},
		"A. B. C. 12":                             {"ABC-12"},
		"what changed on A R S T fifteen hundred": nil,
		"X V six evals":                           nil,
		"X V 6 evals":                             nil,
		"a b testing":                             nil,
	} {
		if got := issueKeysIn(readKeys(t, r, in)); !slices.Equal(got, want) {
			t.Errorf("issueKeysIn(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"what's happening with X V six evals?": "xv6 evals",
		"what's happening with X V 6 evals?":   "xv6 evals",
		"what's happening with A B C thirteen": "",
	} {
		if got := searchTerms(readKeys(t, r, in)); got != want {
			t.Errorf("searchTerms(%q) = %q, want %q", in, got, want)
		}
	}
}
