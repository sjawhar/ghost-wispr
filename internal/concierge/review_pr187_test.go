package concierge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// Sol's review of #187 at d3e076e0
// (https://github.com/sjawhar/ghost-wispr/pull/187#pullrequestreview-6035440730):
// its four findings, and the controls it ran, as tests.

// TestReview187SearchControls: a name spelled out is searched as the word it
// spells, its possessive included; letters, digits, contractions, dates and
// money that spell no name stay as they were.
func TestReview187SearchControls(t *testing.T) {
	for in, want := range map[string]string{
		"what's happening with ex v six evals?":     "xv6 evals",
		"what's happening with x v 6 evals?":        "xv6 evals",
		"what's happening with ex-v-six evals?":     "xv6 evals",
		"what's happening with ex v six's evals?":   "xv6 evals",
		"what's happening with x v 6's evals?":      "xv6 evals",
		"a b testing":                               "testing",
		"I or a":                                    "",
		"part two b":                                "part",
		"C plus plus":                               "plus",
		"what's one meeting voice update?":          "meeting voice",
		"what’s one meeting voice update?":          "meeting voice",
		"the team's one on one notes":               "team notes",
		"how is INFRA-4592":                         "",
		"the budget is $100 on 11/24/2026":          "budget",
		"what is happening with the Parser Evals?":  "parser evals",
		"what is happening with the parser-evals?":  "parser evals",
		"what's happening with the x v six release": "xv6 release",
	} {
		if got := searchTerms(in); got != want {
			t.Errorf("searchTerms(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"one two three", "six seven", "November twenty four", "$6 b", "I or a", "C plus plus", "what's one", "team's one", "six's"} {
		if got := spelledOut(in); got != in {
			t.Errorf("spelledOut(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestReview187TitleMatch (findings 3 and 4): a title holds a word searched
// when one of its words is that word or its regular plural, either way ("y"
// to "ies" included), and a stopword or a word under three letters on
// either side holds nothing: "us" is no "uses".
func TestReview187TitleMatch(t *testing.T) {
	for _, tc := range []struct {
		title, terms string
		want         bool
	}{
		{"XV6: kernel evals", "xv6", true},
		{"Parser-evals", "eval", true},
		{"Parser eval", "evals", true},
		{"Build boxes", "box", true},
		{"Security policies", "policy", true},
		{"Security policy", "policies", true},
		{"Class matches", "match", true},
		{"Wishes", "wish", true},
		{"Use cases", "uses", true},
		{"A budget meeting", "xv6", false},
		{"XV60 evals", "xv6", false},
		{"what is happening with the issue", "xv6", false},
		{"Tell us about the budget", "uses xv6", false},
		{"Tell us about the budget", "us", false},
		{"Go to the box", "to", false},
	} {
		if got := titled(tc.title, tc.terms); got != tc.want {
			t.Errorf("titled(%q, %q) = %v, want %v", tc.title, tc.terms, got, tc.want)
		}
	}
}

// TestReview187AStopwordKeepsNoIssue (finding 3): an issue whose title holds
// a searched word only as a stopword's plural is not kept.
func TestReview187AStopwordKeepsNoIssue(t *testing.T) {
	r := newRig(t)
	r.concierge.fullWait = 40 * time.Millisecond
	r.records.searchDelay = time.Hour
	r.records.kwHits = []dispatch.SearchHit{
		{Kind: "issue", IssueKey: "ABC-12", Title: "Tell us about the budget", Score: 0.5},
		{Kind: "issue", IssueKey: "ABC-13", Title: "XV6 kernel", Score: 0.49},
	}
	if said := answerTurn(t, r, "who uses xv6?"); len(said) != 1 || said[0].text != "Did you mean A B C thirteen?" {
		t.Errorf("spoken = %+v, want only the issue whose title names xv6", said)
	}
}

// TestReview187APluralInYKeepsItsIssue (finding 4): "policies" holds
// "policy".
func TestReview187APluralInYKeepsItsIssue(t *testing.T) {
	r := newRig(t)
	r.concierge.fullWait = 40 * time.Millisecond
	r.records.searchDelay = time.Hour
	r.records.kwHits = []dispatch.SearchHit{
		{Kind: "issue", IssueKey: "ABC-12", Title: "Security policies", Score: 0.5},
		{Kind: "issue", IssueKey: "ABC-13", Title: "Office lights", Score: 0.49},
	}
	if said := answerTurn(t, r, "what is happening with the policy?"); len(said) != 1 || said[0].text != "Did you mean A B C twelve?" {
		t.Errorf("spoken = %+v, want the security policies issue", said)
	}
}

// TestReview187TimelyFullResultControl: a full result in time is offered as
// it came, unfiltered.
func TestReview187TimelyFullResultControl(t *testing.T) {
	r := newRig(t)
	r.records.searchDelay = 10 * time.Millisecond
	r.records.hits = []dispatch.SearchHit{{IssueKey: "ABC-12", Title: "Budget", Score: 0.5}, {IssueKey: "ABC-13", Title: "Lights", Score: 0.49}}
	r.records.kwHits = []dispatch.SearchHit{{IssueKey: "ABC-14", Title: "XV6", Score: 0.5}, {IssueKey: "ABC-15", Title: "XV6", Score: 0.49}}
	if said := answerTurn(t, r, "what is happening with xv6?"); len(said) != 1 || said[0].text != "Did you mean A B C twelve, or A B C thirteen?" {
		t.Errorf("spoken = %+v, want the full result's top two", said)
	}
}

// TestReview187OneIssueReplies (findings 1 and 2): of one issue offered, only
// a whole reply that confirms picks it: a yes form, maybe with a filler, or
// its key alone or with "please" or "that one"; a no form, a negation with
// the key included, picks none; a question, a number or any other word makes
// the turn a new question.
func TestReview187OneIssueReplies(t *testing.T) {
	for _, tc := range []struct {
		reply, key string
		ok         bool
	}{
		{"yes", "ABC-12", true},
		{"Yes.", "ABC-12", true},
		{"Yeah, that one.", "ABC-12", true},
		{"Yes please", "ABC-12", true},
		{"That one.", "ABC-12", true},
		{"The first one", "ABC-12", true},
		{"Uh, yep.", "ABC-12", true},
		{"Sure, that's it.", "ABC-12", true},
		{"ABC-12, please", "ABC-12", true},
		{"ABC 12", "ABC-12", true},
		{"ABC-12, that one.", "ABC-12", true},
		{"no", "", true},
		{"Nope, not that one.", "", true},
		{"No, not ABC-12.", "", true},
		{"What is blocking ABC-12?", "", false},
		{"what is blocking ABC-12", "", false},
		{"Is that right?", "", false},
		{"is that right", "", false},
		{"That's 100, right?", "", false},
		{"that's 100 right", "", false},
		{"yes and no", "", false},
		{"what about the tests?", "", false},
		{"the first item in our budget is rent", "", false},
	} {
		r := newRig(t)
		r.concierge.offerPick(&r.exchange, "ask the agent on the realtime voice whether the tap is merged", []candidate{{"ABC-12", "Realtime voice"}})
		_, key, ok := r.concierge.takePick(&r.exchange, tc.reply)
		if key != tc.key || ok != tc.ok {
			t.Errorf("takePick(%q) = %q, %v; want %q, %v", tc.reply, key, ok, tc.key, tc.ok)
		}
	}
}

// TestReview187TwoIssueKeyReplies (finding 2): of two issues offered, a key
// picks only as a confirmation; a new question that holds a key is a new
// question, and a negation with a key picks none.
func TestReview187TwoIssueKeyReplies(t *testing.T) {
	for _, tc := range []struct {
		reply, key string
		ok         bool
	}{
		{"ABC 13", "ABC-13", true},
		{"ABC-13, please", "ABC-13", true},
		{"what is blocking ABC-13?", "", false},
		{"No, not ABC-13.", "", true},
	} {
		r := newRig(t)
		r.concierge.offerPick(&r.exchange, "ask the agent on the realtime voice whether the tap is merged", []candidate{{"ABC-12", "Realtime voice"}, {"ABC-13", "Wake word"}})
		_, key, ok := r.concierge.takePick(&r.exchange, tc.reply)
		if key != tc.key || ok != tc.ok {
			t.Errorf("takePick(%q) = %q, %v; want %q, %v", tc.reply, key, ok, tc.key, tc.ok)
		}
	}
}

// TestReview187AKeyQuestionKeepsItsWords (finding 2): a new question that
// names the offered key goes to its holder in its own words, not as the
// question first asked.
func TestReview187AKeyQuestionKeepsItsWords(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
	r.concierge.offerPick(&r.exchange, "ask the agent on the realtime voice whether the tap is merged", []candidate{{"ABC-12", "Realtime voice"}})
	_ = answerTurn(t, r, "What is blocking ABC-12?")
	r.agents.mu.Lock()
	defer r.agents.mu.Unlock()
	if len(r.agents.sent) != 1 || !strings.Contains(r.agents.sent[0].Message, "What is blocking ABC-12?") {
		t.Errorf("sent %+v, want one question in its own words", r.agents.sent)
	}
}

// TestReview187SideTalkSendsNoQuestion (finding 1): through the real
// Supervisor, after the one-issue confirmation of a bare number, only a yes
// sends the question first asked.
func TestReview187SideTalkSendsNoQuestion(t *testing.T) {
	for _, reply := range []string{"yes", "the first item in our budget is rent", "No, not ABC-4592.", "Is that right?", "That's 100, right?"} {
		t.Run(reply, func(t *testing.T) {
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
			r.hear(reply)
			r7Until(t, "follow-up completed", func() bool { o, c := r.voice.counts(); return o == 1 && c == 1 })
			r.agents.mu.Lock()
			sent := len(r.agents.sent)
			r.agents.mu.Unlock()
			want := 0
			if reply == "yes" {
				want = 1
			}
			if sent != want {
				t.Errorf("sent %d question(s), want %d", sent, want)
			}
		})
	}
}
