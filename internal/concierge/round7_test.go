package concierge

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// unclearSearch is a search for "the realtime voice" whose best issue, ABC-12
// (held by live session agent-7), leads ABC-13 by less than 1.5 times.
func unclearSearch(r *rig) {
	heldIssue(r, "session", false)
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "done"}
	r.records.hits = []dispatch.SearchHit{
		{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.007},
		{Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Score: 0.006},
	}
}

// TestAnUnclearSearchOffersTheTopTwo (c-find, R5): a search with no one clear
// match makes the voice ask, in one line, which of the top two issues the
// person meant, and do nothing else: no model call and no ask, and nothing
// withheld silently.
func TestAnUnclearSearchOffersTheTopTwo(t *testing.T) {
	r := newRig(t)
	unclearSearch(r)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	spoken := answerTurn(t, r, "ask the agent on the realtime voice whether the tap is merged")
	if want := "Did you mean ABC-12, Realtime voice, or ABC-13, Wake word?"; len(spoken) != 1 || spoken[0].text != want {
		t.Fatalf("spoken = %+v, want only %q", spoken, want)
	}
	if len(r.model.requests) != 0 || len(r.agents.sent) != 0 {
		t.Fatalf("model calls %d, sent %d; want none until the person picks", len(r.model.requests), len(r.agents.sent))
	}
}

// TestAPickAnswersTheQuestionAsked (c-find, R5; c-ask-holder, R7): the next
// turn of the same exchange picks one of the two, by its place or its key, and
// the Concierge answers the question first asked about that issue: here it
// asks its holder.
func TestAPickAnswersTheQuestionAsked(t *testing.T) {
	for _, c := range []struct{ pick, key, holder string }{
		{"the first one", "ABC-12", "agent-7"},
		{"the second", "ABC-13", "agent-9"},
		{"ABC 13", "ABC-13", "agent-9"},
	} {
		r := newRig(t)
		unclearSearch(r)
		r.records.issues["ABC-13"].Claim = &dispatch.Claim{Actor: actor("session", "agent-9", "Other")}
		r.agents.states["agent-9"] = envoy.SessionInfo{SessionID: "agent-9", State: envoy.StateLive}
		answerTurn(t, r, "ask the agent on the realtime voice whether the tap is merged")
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is open."))}}
		spoken := answerTurn(t, r, c.pick)
		if len(r.agents.sent) != 1 || r.agents.sent[0].Target != c.holder || !strings.Contains(r.agents.sent[0].Message, "whether the tap is merged") {
			t.Errorf("%q: sent %+v, want the first question to %s", c.pick, r.agents.sent, c.holder)
			continue
		}
		if last := spoken[len(spoken)-1].text; !strings.Contains(last, "asked the agent on "+c.key) {
			t.Errorf("%q: spoken = %+v, want the asked line for %s", c.pick, spoken, c.key)
		}
	}
}

// TestAnAnswerMakesAtMostTwoSearches (c-fast-answer, R6): the read's search
// and the model's together are at most two in one answer.
func TestAnAnswerMakesAtMostTwoSearches(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "none", false)
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "done"}
	r.records.hits = []dispatch.SearchHit{
		{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.009},
		{Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Score: 0.001},
	}
	search := func(id string) []*genai.GenerateContentResponse {
		return []*genai.GenerateContentResponse{chunk(call(id, "search_issues", map[string]any{"query": "voice"}, ""))}
	}
	r.model.turns = [][]*genai.GenerateContentResponse{search("a"), search("b"), search("c"), {chunk(text("PR 80 is in review."))}}
	answerTurn(t, r, "what is happening with the realtime voice")
	if n := len(r.records.searches()); n > 2 {
		t.Fatalf("Dispatch was searched %d times in one answer, want at most 2", n)
	}
}

// TestNoFactBySixSecondsSaysWhatIsKnown (c-fast-answer, R6): with no fact 6 s
// after the turn, the model stops and the voice says what is known: the
// status the record shows, or that it found no issue. It runs into no bound
// and says no apology.
func TestNoFactBySixSecondsSaysWhatIsKnown(t *testing.T) {
	if knownAfter != 6*time.Second {
		t.Fatalf("knownAfter = %v, want 6 s", knownAfter)
	}
	for _, c := range []struct{ turn, want string }{
		{"what is happening with ABC-12", "ABC-12 is in progress."},
		{"what about it", noIssueFound},
	} {
		r := newRig(t)
		r.concierge.knownBound = 300 * time.Millisecond
		heldIssue(r, "none", false)
		r.model.block = make(chan struct{})
		start := time.Now()
		spoken := answerTurn(t, r, c.turn)
		close(r.model.block)
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%q: the answer took %v, want it cut at the 300 ms mark", c.turn, took)
		}
		if len(spoken) == 0 || spoken[0].text != c.want {
			t.Errorf("%q: spoken = %+v, want %q first", c.turn, spoken, c.want)
		}
	}
}
