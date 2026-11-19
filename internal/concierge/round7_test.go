package concierge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
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

// TestAnEmptySearchSpendsNoSearch (R6): a search the model asks with no
// words never reaches Dispatch, so it spends none of the answer's two.
func TestAnEmptySearchSpendsNoSearch(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "none", false)
	search := func(id, query string) []*genai.GenerateContentResponse {
		return []*genai.GenerateContentResponse{chunk(call(id, "search_issues", map[string]any{"query": query}, ""))}
	}
	r.model.turns = [][]*genai.GenerateContentResponse{search("a", ""), search("b", ""), search("c", "voice"), {chunk(text("PR 80 is in review."))}}
	answerTurn(t, r, "what is happening with ABC-12")
	if n := len(r.records.searches()); n != 1 {
		t.Fatalf("Dispatch was searched %d times, want once: the empty searches spend nothing", n)
	}
}

// TestASlowSearchIsNoOutage (R6, R10): Dispatch's normal search takes 1 to
// 6 s, measured live, so a search past a read's bound is slow, not out of
// reach. One within its own bound is used; one past it has found nothing
// yet, so the voice asks for the key, and no error sound plays.
func TestASlowSearchIsNoOutage(t *testing.T) {
	if searchBound != 5*time.Second {
		t.Fatalf("searchBound = %v, want 5 s", searchBound)
	}
	r := newRig(t)
	r.concierge.callBound, r.concierge.searchBound = 100*time.Millisecond, 600*time.Millisecond
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	r.records.searchDelay = 300 * time.Millisecond
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
	if err != nil || len(spoken) == 0 || spoken[len(spoken)-1].text != "PR 80 is in review." {
		t.Fatalf("a search slower than a read's bound: %v, spoken %+v; want the fact", err, spoken)
	}

	r = newRig(t)
	r.concierge.callBound, r.concierge.searchBound = 100*time.Millisecond, 300*time.Millisecond
	unclearSearch(r)
	r.records.searchDelay = time.Hour
	spoken = nil
	err = r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
	if err != nil || len(spoken) == 0 || spoken[len(spoken)-1].text != noIssueFound {
		t.Fatalf("a search past its bound: %v, spoken %+v; want %q and no error", err, spoken, noIssueFound)
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

// TestSearchTermsTakeTheDescription: a turn that describes the issue ("the
// issue about realtime spoken conversation") searches the description's
// words alone, not the rest of the question.
func TestSearchTermsTakeTheDescription(t *testing.T) {
	for turn, want := range map[string]string{
		"please ask the agent who holds the issue about realtime spoken conversation when slice one will be ready to try.": "realtime spoken conversation",
		"what is happening with the issue on the wake word spotter?":                                                       "wake word spotter",
		"what is happening with the realtime voice":                                                                        "realtime voice",
	} {
		if got := searchTerms(turn); got != want {
			t.Errorf("searchTerms(%q) = %q, want %q", turn, got, want)
		}
	}
}

// TestDidYouMeanCutsLongTitles (R5, R8): the question which issue names each
// by its key and the start of its title, in the ear's form.
func TestDidYouMeanCutsLongTitles(t *testing.T) {
	line := didYouMean([]candidate{
		{"INFRA-4592", "Ghost Wispr: realtime spoken conversation with any omp agent"},
		{"INFRA-1027", "An office meeting recorder with a light that shows it records, a pause button, and a consent rule"},
	})
	if !strings.Contains(line, "INFRA-4592, Ghost Wispr") || !strings.Contains(line, "INFRA-1027, An office meeting") || earForm(line) != "" {
		t.Fatalf("didYouMean = %q (form %q), want both keys with the start of each title", line, earForm(line))
	}
}

// TestThePromptForbidsAGuessedWhen (R6): the model is told never to guess
// when or whether, and, when the recorder asks the holder, that it does.
func TestThePromptForbidsAGuessedWhen(t *testing.T) {
	if !strings.Contains(systemPrompt, "never guess a date, a time or a duration") {
		t.Errorf("the system prompt does not forbid a guessed when")
	}
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	answerTurn(t, r, "ask the agent on ABC-12 when slice one will be ready")
	if len(r.model.requests) == 0 || !strings.Contains(r.model.requests[0][0].Parts[0].Text, "The recorder is asking the agent that holds ABC-12") {
		t.Fatalf("the prompt of an asked question does not say the recorder asks the holder")
	}
}

// TestAHungEnvoyOrDispatchEndsAtItsBound (R6, R10): an Envoy lookup or a
// Dispatch read that takes the connection and never answers ends at its own
// bound, well inside the answer's 8 s: the voice says it cannot reach it,
// speaks what Dispatch did give, and the error wraps voice.ErrUnreachable.
func TestAHungEnvoyOrDispatchEndsAtItsBound(t *testing.T) {
	if lookupBound != 2*time.Second || callBound != 3*time.Second {
		t.Fatalf("bounds = %v and %v, want 2 s and 3 s", lookupBound, callBound)
	}
	r := newRig(t)
	r.concierge.lookupBound = 200 * time.Millisecond
	heldIssue(r, "session", false)
	r.agents.lookupHang = true
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	var spoken []said
	start := time.Now()
	err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with ABC-12", recorder(&spoken))
	if took := time.Since(start); took > 1500*time.Millisecond || !errors.Is(err, voice.ErrUnreachable) {
		t.Fatalf("a hung Envoy: Answer = %v after %v, want voice.ErrUnreachable at the 200 ms bound", err, took)
	}
	if !slices.ContainsFunc(spoken, func(s said) bool { return s.text == "PR 80 is in review." }) || !slices.ContainsFunc(spoken, func(s said) bool { return s.text == "I cannot reach ABC-12's agent yet." }) {
		t.Fatalf("a hung Envoy: spoken = %+v, want the fact and the cannot-reach line", spoken)
	}

	r = newRig(t)
	r.concierge.callBound = 200 * time.Millisecond
	heldIssue(r, "none", false)
	r.records.block = make(chan struct{})
	defer close(r.records.block)
	spoken = nil
	start = time.Now()
	err = r.concierge.Answer(context.Background(), &r.exchange, "what is happening with ABC-12", recorder(&spoken))
	if took := time.Since(start); took > 1500*time.Millisecond || !errors.Is(err, voice.ErrUnreachable) {
		t.Fatalf("a hung Dispatch: Answer = %v after %v, want voice.ErrUnreachable at the 200 ms bound", err, took)
	}
	if len(spoken) == 0 || spoken[0].text != cannotReachDispatch {
		t.Fatalf("a hung Dispatch: spoken = %+v, want %q", spoken, cannotReachDispatch)
	}
}

// TestARefusedQuestionIsNoOutage (R10): Envoy that answers and refuses the
// question is reachable: the voice says it could not ask, and the answer
// carries no error, so no error sound plays.
func TestARefusedQuestionIsNoOutage(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.agents.sendErr = &envoy.ListenerError{Path: "/v1/messages/send", Status: 403, Message: "refused"}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	spoken := answerTurn(t, r, "ask the agent on ABC-12 whether the tap is merged")
	if last := spoken[len(spoken)-1].text; last != "I could not ask the agent on ABC-12." {
		t.Fatalf("spoken = %+v, want the could-not-ask line last", spoken)
	}
}

// TestAFastAnswerChecksANameAfterAnAbbreviation (R8): a fast answer's
// sentences are cut where earCheck cuts them, so a name after "e.g." is
// checked like any other.
func TestAFastAnswerChecksANameAfterAnAbbreviation(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "none", false)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("It is in review, e.g. Alex saw it."))},
		{chunk(text("PR 80 is in review."))},
	}
	for _, s := range answerTurn(t, r, "what is happening with ABC-12") {
		if strings.Contains(s.text, "Alex") {
			t.Fatalf("a name the record does not hold was spoken after an abbreviation: %q", s.text)
		}
	}
}

// TestEarCheckReadsHyphensYearsAndTimes (R8): "three-hour" is a figure; a
// unit after a unit starts a new figure ("seven, eight" is 7 and 8); a year
// and a time said in words are held by their digits.
func TestEarCheckReadsHyphensYearsAndTimes(t *testing.T) {
	const source = "A 4-hour outage at 5:30 in 2026, over two-week sprints; rows 7, 8 failed."
	for _, c := range []struct {
		text  string
		fails bool
	}{
		{"It was a three-hour outage.", true},
		{"It was a four-hour outage.", false},
		{"It started at five thirty.", false},
		{"It was in twenty twenty-six.", false},
		{"Rows seven, eight failed.", false},
		{"It ran over two week sprints.", false},
	} {
		if reason := earCheck(c.text, source, ""); (reason != "") != c.fails {
			t.Errorf("earCheck(%q) = %q, want failing %v", c.text, reason, c.fails)
		}
	}
}

// TestARewriteIsToldTheKeyAndKeepsTheForm (R8 as written): the rewrite is
// told the question's key, and the spoken text, key included, has R8's form:
// a 20-word rewrite that the key would push to 21 words is written again.
func TestARewriteIsToldTheKeyAndKeepsTheForm(t *testing.T) {
	r := newRig(t)
	twenty := "The " + strings.Repeat("tap ", 18) + "merged."
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text(twenty))}, {chunk(text("ABC-12: the tap merged."))}}
	spoken := r.concierge.forEar(context.Background(), "The tap merged.", "ABC-12")
	if spoken != "ABC-12: the tap merged." || earForm(spoken) != "" {
		t.Fatalf("spoken = %q (form %q), want the second rewrite", spoken, earForm(spoken))
	}
	if !strings.Contains(r.model.requests[0][0].Parts[0].Text, "ABC-12") {
		t.Fatalf("the rewrite was not told the question's key")
	}
}

// TestAFailedSayKeepsTheOutage (R10): when Dispatch is out of reach and the
// voice fails to say so, Answer's error still wraps voice.ErrUnreachable.
func TestAFailedSayKeepsTheOutage(t *testing.T) {
	r := newRig(t)
	r.records.err = dispatch.ErrUnreachable
	failing := func(context.Context, string, voice.SayMode) error { return errors.New("voice: session failed") }
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what about ABC-12", failing); !errors.Is(err, voice.ErrUnreachable) {
		t.Fatalf("Answer = %v, want it to wrap voice.ErrUnreachable", err)
	}
}

// TestAReplaceLogsAReplace: the notice for a replaced question logs that it
// was replaced, not cancelled.
func TestAReplaceLogsAReplace(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	r.name(t, "agent-7")
	r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": "is the tap merged"})
	r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": "who reviews it"})
	if !strings.Contains(logs.String(), `msg="concierge told an agent its question was replaced"`) {
		t.Fatalf("the log lacks the replace line:\n%s", logs)
	}
}
