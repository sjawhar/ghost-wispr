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

// TestAClearKeywordMatchAnswersAtOnce (R5, R6): each search runs the
// keyword-only search and the full one at once. A keyword result with one
// clear match is taken at once, and the full search is stopped; together
// they are one search.
func TestAClearKeywordMatchAnswersAtOnce(t *testing.T) {
	r := newRig(t)
	unclearSearch(r)
	r.records.kwHits = []dispatch.SearchHit{
		{Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Score: 0.9},
		{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.1},
	}
	r.records.searchDelay = time.Hour
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is done."))}}
	start := time.Now()
	spoken := answerTurn(t, r, "what is happening with the wake word")
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the answer took %v, want the keyword result at once", took)
	}
	if len(spoken) < 1 || spoken[0].text != "I took ABC-13." {
		t.Fatalf("spoken = %+v, want the keyword match first", spoken)
	}
	// The full search starts beside the keyword one; its goroutine may run
	// after the answer, once the clear keyword result stopped it.
	eventually(t, "the full search started", func() bool { return len(r.records.searches()) == 1 })
	if kw, full := len(r.records.kwQueries), len(r.records.searches()); kw != 1 || full != 1 {
		t.Fatalf("keyword searches %d, full %d; want one of each", kw, full)
	}
}

// TestAnUnclearKeywordMatchTakesTheFullSearch (R5, R6): when the keyword
// result has no one clear match, or fails, the full search's result is taken.
// A full search past its bound falls back to the keyword result's issues
// whose title holds a word searched: here only ABC-12, "Realtime voice".
func TestAnUnclearKeywordMatchTakesTheFullSearch(t *testing.T) {
	for _, c := range []struct {
		name string
		kw   []dispatch.SearchHit
		err  error
		want string
	}{
		{"unclear keyword result", []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Score: 0.5}, {Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.4}}, nil, "I took ABC-12."},
		{"no keyword result", nil, nil, "I took ABC-12."},
		{"keyword search failed", nil, errors.New("dispatch: 500"), "I took ABC-12."},
	} {
		r := newRig(t)
		unclearSearch(r)
		r.records.hits[0].Score = 0.012
		r.records.kwHits, r.records.kwErr = c.kw, c.err
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
		spoken := answerTurn(t, r, "what is happening with the realtime voice")
		if len(spoken) == 0 || spoken[0].text != c.want {
			t.Errorf("%s: spoken = %+v, want %q first", c.name, spoken, c.want)
		}
	}
	r := newRig(t)
	r.concierge.searchBound = 200 * time.Millisecond
	unclearSearch(r)
	r.records.kwHits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Score: 0.5}, {Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.4}}
	r.records.searchDelay = time.Hour
	spoken := answerTurn(t, r, "what is happening with the realtime voice")
	if len(spoken) != 1 || spoken[0].text != "Did you mean A B C twelve?" {
		t.Fatalf("a full search past its bound: spoken = %+v, want the keyword result's issue the words name", spoken)
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
	if want := "Did you mean A B C twelve, or A B C thirteen?"; len(spoken) != 1 || spoken[0].text != want {
		t.Fatalf("spoken = %+v, want only %q", spoken, want)
	}
	if len(r.model.requests) != 0 || len(r.agents.sent) != 0 {
		t.Fatalf("model calls %d, sent %d; want none until the person picks", len(r.model.requests), len(r.agents.sent))
	}
}

// TestAPickAnswersTheQuestionAsked (c-find, R5; c-ask-holder, R7): the next
// turn of the same exchange picks one of the two, by its place, its key, or
// words of exactly one title, and the Concierge answers the question first
// asked about that issue: here it asks its holder.
func TestAPickAnswersTheQuestionAsked(t *testing.T) {
	for _, c := range []struct{ pick, key, holder string }{
		{"the first one", "ABC-12", "agent-7"},
		{"the second", "ABC-13", "agent-9"},
		{"ABC 13", "ABC-13", "agent-9"},
		{"the one about the wake word", "ABC-13", "agent-9"},
		{"wake word", "ABC-13", "agent-9"},
	} {
		r := newRig(t)
		unclearSearch(r)
		r.records.issues["ABC-13"].Claim = &dispatch.Claim{Actor: actor("session", "agent-9", "Other")}
		r.agents.states["agent-9"] = envoy.SessionInfo{SessionID: "agent-9", State: envoy.StateLive}
		answerTurn(t, r, "ask the agent on the realtime voice whether the tap is merged")
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is open."))}}
		spoken := answerTurn(t, r, c.pick)
		if len(r.agents.sent) != 1 || r.agents.sent[0].Target != c.holder || !strings.Contains(r.agents.sent[0].Message, ": Is the tap merged?\n") {
			t.Errorf("%q: sent %+v, want the first question to %s", c.pick, r.agents.sent, c.holder)
			continue
		}
		if last := spoken[len(spoken)-1].text; !strings.Contains(last, "asked the agent on "+c.key) {
			t.Errorf("%q: spoken = %+v, want the asked line for %s", c.pick, spoken, c.key)
		}
	}
}

// TestAReplyThatPicksNothingIsItsOwnQuestion (c-find, R5): "one" alone picks
// nothing, nor do words both titles share or words of neither; such a turn
// is a question of its own, and asks nobody unasked.
func TestAReplyThatPicksNothingIsItsOwnQuestion(t *testing.T) {
	for _, reply := range []string{"that one", "the word one", "never mind the tap"} {
		r := newRig(t)
		unclearSearch(r)
		r.records.issues["ABC-12"].Title, r.records.hits[0].Title = "Voice word", "Voice word"
		answerTurn(t, r, "ask the agent on the realtime voice whether the tap is merged")
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is open."))}}
		answerTurn(t, r, reply)
		for _, m := range r.agents.sent {
			if strings.Contains(m.Message, "the tap merged") {
				t.Errorf("%q picked an issue: sent %+v", reply, m)
			}
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
		{"what is happening with ABC-12", "ABC-12 is in progress, updated 5 minutes ago."},
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
// issue about realtime spoken conversation") gives the keyword search the
// description's words alone, not the rest of the question.
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

// TestDidYouMeanSaysNoTitle (R5, R8): the question which issue names each by
// its key as it is spoken, however long its title, in the ear's form.
func TestDidYouMeanSaysNoTitle(t *testing.T) {
	line := didYouMean([]candidate{
		{"INFRA-4592", "Ghost Wispr: realtime spoken conversation with any omp agent"},
		{"INFRA-1027", "An office meeting recorder with a light that shows it records, a pause button, and a consent rule"},
	})
	if line != "Did you mean Infra forty-five ninety-two, or Infra ten twenty-seven?" || earForm(line) != "" {
		t.Fatalf("didYouMean = %q (form %q), want both keys as spoken and no title", line, earForm(line))
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
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
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
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
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

// TestASearchedIssueIsNamedFirst (c-fast-answer, R6): when a search chose the
// issue, with one clear match, the answer names it, then gives the facts.
func TestASearchedIssueIsNamedFirst(t *testing.T) {
	r := newRig(t)
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	spoken := answerTurn(t, r, "what is happening with the realtime voice")
	if len(spoken) != 2 || spoken[0].text != "I took ABC-12." || spoken[1].text != "PR 80 is in review." {
		t.Fatalf("spoken = %+v, want the issue, then the fact", spoken)
	}
}

// TestAHungProjectListIsAnOutage (R10): the read of Dispatch's projects ends
// at a read's bound. With an older list kept, the key is read, which ends at
// its own bound; with none, a key-shaped word cannot be told, and Dispatch is
// out of reach. Either way the room hears so, with the error sound, well
// inside the answer's 8 s.
func TestAHungProjectListIsAnOutage(t *testing.T) {
	for _, known := range []bool{true, false} {
		r := newRig(t)
		r.concierge.callBound = 200 * time.Millisecond
		heldIssue(r, "none", false)
		r.records.mu.Lock()
		r.records.projectsBlock = make(chan struct{})
		r.records.block = make(chan struct{})
		r.records.mu.Unlock()
		r.concierge.mu.Lock()
		r.concierge.projectsRead = time.Time{}
		if !known {
			r.concierge.projects = nil
		}
		r.concierge.mu.Unlock()
		var spoken []said
		start := time.Now()
		err := r.concierge.Answer(context.Background(), &r.exchange, "what about ABC-12", recorder(&spoken))
		took := time.Since(start)
		close(r.records.projectsBlock)
		close(r.records.block)
		if took > 1500*time.Millisecond || !errors.Is(err, voice.ErrUnreachable) {
			t.Fatalf("list known %v: Answer = %v after %v, want voice.ErrUnreachable within the bounds", known, err, took)
		}
		if len(spoken) == 0 || spoken[len(spoken)-1].text != cannotReachDispatch {
			t.Fatalf("list known %v: spoken = %+v, want %q", known, spoken, cannotReachDispatch)
		}
	}
}

// TestWhoHoldsItIsSaid (R6): the model may say who holds the issue when the
// question asks it; otherwise it leaves that to the recorder.
func TestWhoHoldsItIsSaid(t *testing.T) {
	if !strings.Contains(systemPrompt, "Do not say who holds the issue unless the question asks who holds it.") {
		t.Fatal("the system prompt forbids saying the holder even when asked who holds it")
	}
}

// TestEarCheckReadsClockTimesAndListsOfTeens (R8): "ten thirty" is 10:30,
// and "twelve, thirteen" is 12 and 13, not the years 1030 and 1213.
func TestEarCheckReadsClockTimesAndListsOfTeens(t *testing.T) {
	const source = "It starts at 10:30 and 11:15; rows 12, 13 failed in 2026."
	for _, c := range []struct {
		text  string
		fails bool
	}{
		{"It starts at ten thirty.", false},
		{"It starts at eleven fifteen.", false},
		{"Rows twelve, thirteen failed.", false},
		{"It failed in twenty twenty-six.", false},
		{"It starts at ten forty.", true},
	} {
		if reason := earCheck(c.text, source, ""); (reason != "") != c.fails {
			t.Errorf("earCheck(%q) = %q, want failing %v", c.text, reason, c.fails)
		}
	}
	if earForm("It runs on Node.js and Next.js.") != "" {
		t.Error("a product name read as a file name")
	}
}

// TestTheReadAheadSearchesOnceSettled (R6): Heard searches only words that
// have settled, at most once a turn, so the read-ahead's searches and the
// answer's are at most two together.
func TestTheReadAheadSearchesOnceSettled(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 150 * time.Millisecond
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	for _, words := range []string{"what is happening with the realtime", "what is happening with the realtime voice"} {
		r.concierge.Heard(context.Background(), &r.exchange, words)
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	r.concierge.Heard(context.Background(), &r.exchange, "what is happening with the realtime voice today now")
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(call("a", "search_issues", map[string]any{"query": "wake word"}, ""))},
		{chunk(call("b", "search_issues", map[string]any{"query": "voice"}, ""))},
		{chunk(text("PR 80 is in review."))},
	}
	answerTurn(t, r, "what is happening with the realtime voice today now")
	if searches := r.records.searches(); len(searches) > 2 || searches[0] != "what is happening with the realtime voice" {
		t.Fatalf("searches = %q, want the settled question first and at most two in all", searches)
	}
}

// TestAnAskWithNoWordsFromTheModelSaysNoSorry (R6, R7): when the holder is
// asked and the model has nothing to add, the room hears the asked line, no
// apology, and the answer has no error.
func TestAnAskWithNoWordsFromTheModelSaysNoSorry(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text(""))}, {chunk(text(""))}}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 whether the tap is merged", recorder(&spoken))
	if err != nil || slices.ContainsFunc(spoken, func(s said) bool { return s.text == sorry }) {
		t.Fatalf("Answer = %v, spoken %+v; want no error and no apology", err, spoken)
	}
	if last := spoken[len(spoken)-1].text; !strings.Contains(last, "asked the agent on ABC-12") {
		t.Fatalf("spoken = %+v, want the asked line last", spoken)
	}
}

// TestTheRoutingLineSaysAnAskNotAboutStatus (R6): a question not about
// status is asked of the live holder as soon as the holder is known, so the
// routing line says so while the rest of the record is read.
func TestTheRoutingLineSaysAnAskNotAboutStatus(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	issue := r.records.issues["ABC-12"]
	issue.PrimaryArtifactID = "doc-1"
	issue.Artifacts = append(issue.Artifacts, struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}{ID: "doc-1", Slug: "spec"})
	r.records.docs["ABC-12/spec"] = "## Summary"
	r.records.docBlock = make(chan struct{})
	go func() {
		time.Sleep(2200 * time.Millisecond)
		close(r.records.docBlock)
	}()
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
	spoken := answerTurn(t, r, "is the tap on ABC-12 merged")
	if len(spoken) == 0 || spoken[0].text != "Asking the agent on ABC-12." {
		t.Fatalf("spoken = %+v, want the routing line to say the ask", spoken)
	}
}

// TestAnEnvoyFaultIsAnOutage (R10): Envoy that answers a send with a server
// fault is out of service: the error sound plays. A refusal (4xx) is not.
func TestAnEnvoyFaultIsAnOutage(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.agents.sendErr = &envoy.ListenerError{Path: "/v1/messages/send", Status: 503, Message: "bus unanswered"}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 whether the tap is merged", recorder(&spoken))
	if !errors.Is(err, voice.ErrUnreachable) {
		t.Fatalf("Answer on Envoy's 503 = %v, want voice.ErrUnreachable", err)
	}
}

// TestADocumentIsNamedForTheEar (R8): an event names a document without its
// file extension, so the model never says "spec.md".
func TestADocumentIsNamedForTheEar(t *testing.T) {
	if got := eventText([]byte(`{"name":"spec.md"}`)); got != "spec" {
		t.Fatalf("eventText = %q, want spec", got)
	}
	if got := eventText([]byte(`{"name":"Node.js notes"}`)); got != "Node.js notes" {
		t.Fatalf("eventText = %q, want a name with no file extension kept", got)
	}
}

// TestARetryRepeatsNoSentence (R6): when a sentence fails R8's check and the
// model writes again, a sentence the room already heard is not said twice.
func TestARetryRepeatsNoSentence(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "none", false)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("It is in review. It went to Alex."))},
		{chunk(text("It is in review. PR 80 is in review."))},
	}
	times := 0
	for _, s := range answerTurn(t, r, "what is happening with ABC-12") {
		if s.text == "It is in review." {
			times++
		}
	}
	if times != 1 {
		t.Fatalf("a sentence was said %d times, want once", times)
	}
}

// TestAnAskThatCannotGoIsToldToTheModel (R6, R7): when the person says "ask"
// and the recorder cannot ask the holder, the model is told so, and to say
// only what the record holds.
func TestAnAskThatCannotGoIsToldToTheModel(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.concierge.SetAgents(nil)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	var spoken []said
	_ = r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 when slice one will be ready", recorder(&spoken))
	if len(r.model.requests) == 0 || !strings.Contains(r.model.requests[0][0].Parts[0].Text, "The recorder cannot ask the agent that holds ABC-12 now.") {
		t.Fatalf("the prompt does not say the recorder cannot ask")
	}
}

// TestARetriedQuestionKeepsItsFirstTime (R7): a question sent again after a
// send Envoy did not confirm keeps the time it was first asked, which the
// stored question and the request to reply again both name.
func TestARetriedQuestionKeepsItsFirstTime(t *testing.T) {
	r := newRig(t)
	clock := testNow
	r.concierge.now = func() time.Time { return clock }
	r.name(t, "agent-7")
	ask := map[string]string{"session": "agent-7", "question": "is the tap merged"}
	r.agents.landedErr = errors.New("envoy: context deadline exceeded")
	r.run(t, "ask_agent", ask)
	r.agents.landedErr = nil
	clock = clock.Add(3 * time.Second)
	result := r.run(t, "ask_agent", ask)
	taskID, _ := result["task_id"].(string)
	task, err := r.store.VoiceTask(taskID)
	if err != nil || !task.AskedAt.Equal(testNow) {
		t.Fatalf("the retried task = %+v, %v; want it asked at %v", task, err, testNow)
	}
}

// TestAnAskedQuestionUsesTheFixedSentence (R8): when the model fails R8's
// check twice, the voice says the fixed sentence before the ask line.
func TestAnAskedQuestionUsesTheFixedSentence(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("It went to Alex."))},
		{chunk(text("It went to Blake."))},
	}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 whether the tap is merged", recorder(&spoken))
	if err != nil || len(spoken) != 2 || spoken[0].text != "ABC-12: "+fixedSentence || !strings.Contains(spoken[1].text, "asked the agent on ABC-12") {
		t.Fatalf("Answer = %v, spoken %+v; want the keyed fixed sentence then the asked line", err, spoken)
	}
}

// TestAnUnclearKeywordResultWaitsTwoSecondsAtMost (R5, R6; Main's rule): when
// the keyword result has no one clear match, the full search is waited for
// until 2 s after the turn ended, then the voice asks about the keyword
// result's issues whose title holds a word searched, and says nothing more
// when the full search lands. A full result in time still wins.
func TestAnUnclearKeywordResultWaitsTwoSecondsAtMost(t *testing.T) {
	if fullWaitAfterTurn != 2*time.Second {
		t.Fatalf("fullWaitAfterTurn = %v, want 2 s", fullWaitAfterTurn)
	}
	unclearKeywords := []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Score: 0.5}, {Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.4}}
	r := newRig(t)
	r.concierge.fullWait = 200 * time.Millisecond
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	r.records.kwHits = unclearKeywords
	r.records.searchDelay = 900 * time.Millisecond
	start := time.Now()
	spoken := answerTurn(t, r, "what is happening with the realtime voice")
	if took := time.Since(start); took > 700*time.Millisecond {
		t.Fatalf("the answer took %v, want the question which issue at the 200 ms mark", took)
	}
	time.Sleep(time.Second)
	if len(spoken) != 1 || spoken[0].text != "Did you mean A B C twelve?" {
		t.Fatalf("spoken = %+v, want only the keyword result's issue the words name", spoken)
	}

	r = newRig(t)
	r.concierge.fullWait = 500 * time.Millisecond
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	r.records.kwHits = unclearKeywords
	r.records.searchDelay = 50 * time.Millisecond
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	if spoken := answerTurn(t, r, "what is happening with the realtime voice"); len(spoken) == 0 || spoken[0].text != "I took ABC-12." {
		t.Fatalf("a full result in time: spoken = %+v, want it to win", spoken)
	}
}

// TestAnswerReusesTheReadAheadsSearch (R6; Main's rule): when the final
// turn's question is the one the read-ahead searched, case and punctuation
// aside, Answer takes the read-ahead's search, keyword and full, and
// searches no more.
func TestAnswerReusesTheReadAheadsSearch(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 50 * time.Millisecond
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	r.concierge.Heard(context.Background(), &r.exchange, "what is happening with the realtime voice")
	eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	spoken := answerTurn(t, r, "What is happening with the realtime voice?")
	if full, kw := len(r.records.searches()), len(r.records.kwQueries); full != 1 || kw != 1 {
		t.Fatalf("full searches %d, keyword %d; want the read-ahead's one of each, reused", full, kw)
	}
	if len(spoken) == 0 || spoken[0].text != "I took ABC-12." {
		t.Fatalf("spoken = %+v, want the read-ahead's issue", spoken)
	}
}

// TestAnswerReusesAnEarlyReadAheadsSearch (run 8i): the read-ahead searched
// while the session did not exist yet; the answer, with the session's id and
// the final words, takes that search and searches no more.
func TestAnswerReusesAnEarlyReadAheadsSearch(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 300 * time.Millisecond
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	early := r.exchange
	early.SessionID = ""
	words := strings.Fields("what is happening with the issue about real time spoken conversation?")
	for i := range words {
		r.concierge.Heard(context.Background(), &early, strings.Join(words[:i+1], " "))
		time.Sleep(10 * time.Millisecond)
	}
	eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
	time.Sleep(100 * time.Millisecond)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	answerTurn(t, r, strings.Join(words, " "))
	if full, kw := len(r.records.searches()), len(r.records.kwQueries); full != 1 || kw != 1 {
		t.Fatalf("full searches %q, keyword %q; want the read-ahead's one of each, reused", r.records.searches(), r.records.kwQueries)
	}
}

// TestTheHolderGetsAQuestionInTheAskersIntent (R7, run 8g): the holder gets
// the asker's question as one clean sentence: no routing clause ("who holds
// the issue about …"), an embedded question made direct, Deepgram's
// "SliceOne" said as words; the issue's key goes in the header.
func TestTheHolderGetsAQuestionInTheAskersIntent(t *testing.T) {
	for turn, want := range map[string]string{
		"please ask the agent who holds the issue about real time spoken conversation when SliceOne will be ready to try": "When will slice one be ready to try?",
		"please ask the agent on INFRA 4592. When slice one will be ready to try.":                                        "When will slice one be ready to try?",
		"ask the agent on ABC-12 whether the tap is merged":                                                               "Is the tap merged?",
		"ask the agent on the issue about the cache if it can be dropped":                                                 "Can it be dropped?",
		"is the tap on ABC-12 merged":                  "Is the tap on ABC-12 merged?",
		"ask the agent on ABC-12 why the build failed": "Why the build failed?",
	} {
		if got := fewestWords(turn); got != want {
			t.Errorf("fewestWords(%q) = %q, want %q", turn, got, want)
		}
	}
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
	answerTurn(t, r, "please ask the agent on ABC-12 when SliceOne will be ready to try")
	if len(r.agents.sent) != 1 || !strings.Contains(r.agents.sent[0].Message, "Question about ABC-12: When will slice one be ready to try?\n") {
		t.Fatalf("sent %+v, want the clean question under the key", r.agents.sent)
	}
}

// TestTheQuestionsOwnWordsAreNoInvention (R6, R8; run 8g): a name the asker
// said, such as Deepgram's "SliceOne", is no name the model invented when
// the fast answer says it back.
func TestTheQuestionsOwnWordsAreNoInvention(t *testing.T) {
	const turn = "please ask the agent who holds the issue about real time spoken conversation when SliceOne will be ready to try"
	if reason := earCheckAsked("The record does not say when SliceOne will be ready to try.", "INFRA-4592 is in progress.", turn, ""); reason != "" {
		t.Fatalf("a name from the question was refused: %q", reason)
	}
	if earCheckAsked("It says Blake will try SliceOne.", "INFRA-4592 is in progress.", turn, "") == "" {
		t.Fatal("control: a name neither the record nor the question holds passed")
	}
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: The record does not say when SliceOne will be ready to try."))}}
	spoken := answerTurn(t, r, "please ask the agent on ABC-12 when SliceOne will be ready to try")
	if len(spoken) == 0 || spoken[0].text != "ABC-12: The record does not say when SliceOne will be ready to try." {
		t.Fatalf("spoken = %+v, want the keyed sentence that says the question's name back", spoken)
	}
}

// TestASearchedTurnSaysWhatIsKnownAtTheMark (R5, R6): the line that names
// the issue a search chose is no fact; with no fact by the routing mark it
// plays then, and with no fact by the 6 s mark, the voice still says what is
// known, and the answer has no error. The marks keep their order: 1.5 s,
// then 6 s.
func TestASearchedTurnSaysWhatIsKnownAtTheMark(t *testing.T) {
	r := newRig(t)
	r.concierge.routingAfter, r.concierge.knownBound = 100*time.Millisecond, 400*time.Millisecond
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	r.model.block = make(chan struct{})
	defer close(r.model.block)
	spoken := answerTurn(t, r, "what is happening with the realtime voice")
	if len(spoken) != 2 || spoken[0].mode != voice.Lead || spoken[1].text != "ABC-12 is in progress, updated 5 minutes ago." {
		t.Fatalf("spoken = %+v, want the issue (voice.Lead: it always leads, holds no fact), then what is known", spoken)
	}
}

// TestBothSearchesHungIsAnOutage (R10): the keyword search answers in about
// 0.15 s; when it and the full search both run past their bound, Dispatch is
// out of reach, with the error sound.
func TestBothSearchesHungIsAnOutage(t *testing.T) {
	r := newRig(t)
	r.concierge.searchBound = 200 * time.Millisecond
	unclearSearch(r)
	r.records.kwDelay, r.records.searchDelay = time.Hour, time.Hour
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
	if !errors.Is(err, voice.ErrUnreachable) || len(spoken) == 0 || spoken[len(spoken)-1].text != cannotReachDispatch {
		t.Fatalf("Answer = %v, spoken %+v; want %q and voice.ErrUnreachable", err, spoken, cannotReachDispatch)
	}
}

// TestAKeyReadAheadMakesNoUncountedSearch (R6): a read ahead of a key that
// names no issue reads the key alone and searches nothing uncounted, so the
// answer's search and the model's are at most two in all.
func TestAKeyReadAheadMakesNoUncountedSearch(t *testing.T) {
	r := newRig(t)
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	const turn = "what about abc 99 and the realtime voice"
	r.concierge.Heard(context.Background(), &r.exchange, turn)
	eventually(t, "the read-ahead read the key", func() bool { return slices.Contains(r.records.calls(), "ABC-99") })
	time.Sleep(100 * time.Millisecond)
	search := func(id string) []*genai.GenerateContentResponse {
		return []*genai.GenerateContentResponse{chunk(call(id, "search_issues", map[string]any{"query": "wake word " + id}, ""))}
	}
	r.model.turns = [][]*genai.GenerateContentResponse{search("a"), search("b"), {chunk(text("PR 80 is in review."))}}
	answerTurn(t, r, turn)
	if n := len(r.records.searches()); n > 2 {
		t.Fatalf("searches = %q, want at most two", r.records.searches())
	}
}

// TestTheReadAheadMaySearchAfterNoIssueFound (R6): an answer that found no
// issue takes the read-ahead's count all the same, so the reply, which names
// the issue another way, is searched ahead.
func TestTheReadAheadMaySearchAfterNoIssueFound(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 50 * time.Millisecond
	r.records.hits = nil
	r.concierge.Heard(context.Background(), &r.exchange, "what is happening with the purple submarine")
	eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
	answerTurn(t, r, "what is happening with the purple submarine")
	r.concierge.Heard(context.Background(), &r.exchange, "the realtime voice one")
	eventually(t, "the reply's read-ahead searched", func() bool { return len(r.records.searches()) == 2 })
}

// TestTheReadAheadCountStartsOverEachAnswer (R6): an answer that ends early,
// as "Did you mean" does, still takes the read-ahead's count, so the next
// turn's read-ahead may search; a reply that only picks searches nothing.
func TestTheReadAheadCountStartsOverEachAnswer(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 50 * time.Millisecond
	unclearSearch(r)
	r.concierge.Heard(context.Background(), &r.exchange, "what is happening with the realtime voice")
	eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
	answerTurn(t, r, "what is happening with the realtime voice")
	r.concierge.Heard(context.Background(), &r.exchange, "the second one")
	time.Sleep(200 * time.Millisecond)
	if n := len(r.records.searches()); n != 1 {
		t.Fatalf("a reply to Did you mean searched ahead: searches %q", r.records.searches())
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is done."))}}
	answerTurn(t, r, "the second one")
	r.concierge.Heard(context.Background(), &r.exchange, "and what about the wake word")
	eventually(t, "the next turn's read-ahead searched", func() bool { return len(r.records.searches()) == 2 })
}

// TestAPickByTheWordThatTellsTheTitlesApart (R5): words both offered titles
// share tell nothing; the word only one holds picks it.
func TestAPickByTheWordThatTellsTheTitlesApart(t *testing.T) {
	r := newRig(t)
	r.concierge.offerPick(&r.exchange, "what is the state of the wake word", []candidate{{"ABC-12", "Wake word spotter"}, {"ABC-13", "Wake word false wakes"}})
	if _, key, ok := r.concierge.takePick(&r.exchange, "the wake word spotter"); !ok || key != "ABC-12" {
		t.Fatalf("takePick = %q, %v; want ABC-12", key, ok)
	}
}

// TestAStoppedProjectReadMarksNothingDown (R10): a read of the projects that
// a stop ended is no failure of Dispatch's.
func TestAStoppedProjectReadMarksNothingDown(t *testing.T) {
	r := newRig(t)
	r.records.mu.Lock()
	r.records.projectsBlock = make(chan struct{})
	r.records.mu.Unlock()
	defer close(r.records.projectsBlock)
	r.concierge.mu.Lock()
	r.concierge.projectsBusy = true
	readBefore := r.concierge.projectsRead
	r.concierge.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.concierge.readProjects(ctx)
	r.concierge.mu.Lock()
	down := r.concierge.projectsDown
	r.concierge.mu.Unlock()
	if down {
		t.Fatal("a stopped read marked the projects down")
	}
	r.concierge.mu.Lock()
	readAfter := r.concierge.projectsRead
	r.concierge.mu.Unlock()
	if !readAfter.Equal(readBefore) {
		t.Fatalf("a stopped read set the next read's time from %v to %v", readBefore, readAfter)
	}
}

// TestSearchTermsJoinCompoundsAndDropRoutingVerbs (run 8g): Deepgram writes
// "real time" or "real-time" where titles say "realtime", and "who holds"
// names no work, so a read-ahead never spends its one search on "holds".
func TestSearchTermsJoinCompoundsAndDropRoutingVerbs(t *testing.T) {
	for turn, want := range map[string]string{
		"what is happening with the issue about real time spoken conversation?": "realtime spoken conversation",
		"what is happening with the issue about real-time spoken conversation?": "realtime spoken conversation",
		"please ask the agent who holds":                                        "",
		"who owns the wake word work":                                           "wake word",
	} {
		if got := searchTerms(turn); got != want {
			t.Errorf("searchTerms(%q) = %q, want %q", turn, got, want)
		}
	}
}

// TestThePromptKeepsTheAgentsToolForAgentQuestions (run 8g): the model
// called list_joined_agents first on every turn; it is for questions about
// the agents that joined the meeting.
func TestThePromptKeepsTheAgentsToolForAgentQuestions(t *testing.T) {
	if !strings.Contains(systemPrompt, "Call list_joined_agents only when the question asks which agents joined this meeting.") {
		t.Fatal("the system prompt does not keep list_joined_agents for agent questions")
	}
}

// TestAnAskReadBeforeTheSessionExistedGoesOut (R7; PR #89's review, finding
// 4): "ask the agent on ABC-12 …" heard before the recording session existed
// reads the holder with no session id; the answer, with the id, asks it.
// Exactly one question goes either way.
func TestAnAskReadBeforeTheSessionExistedGoesOut(t *testing.T) {
	const turn = "ask the agent on ABC-12 whether the tap is merged"
	for _, early := range []bool{false, true} {
		r := newRig(t)
		heldIssue(r, "session", false)
		x := r.exchange
		if early {
			x.SessionID = ""
		}
		r.concierge.Heard(context.Background(), &x, turn)
		eventually(t, "the read-ahead of ABC-12", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
		spoken := answerTurn(t, r, turn)
		r.agents.mu.Lock()
		sent := len(r.agents.sent)
		r.agents.mu.Unlock()
		if sent != 1 {
			t.Fatalf("early %v: %d questions went to the holder, want 1; the voice said %+v", early, sent, spoken)
		}
	}
}

// TestEarlyStateGoesWithTheMeetingOrTheGate (PR #89's review): what the
// Concierge read or offered for an exchange heard before its session existed
// holds room words: it goes on any session's end or delete, and on the gate
// closing. The agents named there are only session ids: they go with the
// gate, or after their hour.
func TestEarlyStateGoesWithTheMeetingOrTheGate(t *testing.T) {
	for _, how := range []string{"end", "delete", "gate"} {
		r := newRig(t)
		heldIssue(r, "session", false)
		x := r.exchange
		x.SessionID = ""
		r.concierge.Heard(context.Background(), &x, "ask the agent on ABC-12 whether the tap is merged")
		eventually(t, "the early read named the holder", func() bool { return r.concierge.wasNamed(&x, "agent-7") })
		r.concierge.offerPick(&x, "the realtime voice", []candidate{{"ABC-12", "Realtime voice"}, {"ABC-13", "Wake word"}})
		switch how {
		case "end":
			_ = r.concierge.SessionEnded("meeting-1")
		case "delete":
			r.concierge.SessionDeleted("meeting-1")
		case "gate":
			r.concierge.GateClosed()
		}
		r.concierge.mu.Lock()
		words := len(r.concierge.prefetches) + len(r.concierge.picks)
		names := len(r.concierge.named)
		r.concierge.mu.Unlock()
		if words != 0 || (how == "gate") != (names == 0) {
			t.Errorf("%s: %d early reads and offers left, %d names; want none of the first, and names gone with the gate only", how, words, names)
		}
	}
}

// TestAnotherMeetingsEndKeepsAnEarlyAsk (the fourth review): an exchange
// heard before its session existed, then given it, still asks its holder
// when another meeting ends or is deleted in between.
func TestAnotherMeetingsEndKeepsAnEarlyAsk(t *testing.T) {
	const turn = "ask the agent on ABC-12 whether the tap is merged"
	r := newRig(t)
	heldIssue(r, "session", false)
	early := r.exchange
	early.SessionID = ""
	r.concierge.Heard(context.Background(), &early, turn)
	eventually(t, "the early read named the holder", func() bool { return r.concierge.wasNamed(&early, "agent-7") })
	r.concierge.Heard(context.Background(), &r.exchange, turn)
	r.concierge.SessionDeleted("an-older-meeting")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
	spoken := answerTurn(t, r, turn)
	r.agents.mu.Lock()
	sent := len(r.agents.sent)
	r.agents.mu.Unlock()
	if sent != 1 {
		t.Fatalf("%d questions went to the holder, want 1; the voice said %+v", sent, spoken)
	}
}

// TestAFollowUpTurnIsAskedOnlyWhenItSaysAsk (R7; PR #89's review): a turn
// said in the follow-up window with no wake phrase ("should we tell the
// voice team before the demo?") is answered from the record, and goes to the
// holder only when it says "ask"; a turn with a wake phrase is asked as R7
// says.
func TestAFollowUpTurnIsAskedOnlyWhenItSaysAsk(t *testing.T) {
	for _, c := range []struct {
		name     string
		followUp bool
		turn     string
		asked    bool
	}{
		{"follow-up, not about status", true, "should ABC-12 wait for the demo", false},
		{"follow-up that says ask", true, "ask the agent on ABC-12 whether it should wait for the demo", true},
		{"woken, not about status", false, "should ABC-12 wait for the demo", true},
	} {
		r := newRig(t)
		heldIssue(r, "session", false)
		r.exchange.FollowUpTurn = c.followUp
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
		answerTurn(t, r, c.turn)
		r.agents.mu.Lock()
		asked := len(r.agents.sent) == 1
		r.agents.mu.Unlock()
		if asked != c.asked {
			t.Errorf("%s: asked %v, want %v", c.name, asked, c.asked)
		}
	}
}

// TestTheChosenIssueIsNamedByItsKey (R6): the line that names the issue a
// search chose plays before the first fact, which waits behind it: live, the
// line with the title ("I took INFRA-4592: Ghost Wispr: realtime spoken
// conversation.") held a fact ready at 1.2 s until 7.2 s. It names the key.
func TestTheChosenIssueIsNamedByItsKey(t *testing.T) {
	found := &reading{key: "INFRA-4592", title: "Ghost Wispr: realtime spoken conversation with any omp agent", searched: true, clear: true}
	if lead := found.lead(); len(lead) != 1 || lead[0] != "I took INFRA-4592." {
		t.Fatalf("lead = %q, want the key alone", lead)
	}
}

// TestAReplyThatPicksNothingIsSearchedAfresh (R5; the third review): a reply
// to "Did you mean" that picks neither issue is searched for its own words,
// not answered from the read-ahead of the question before it.
func TestAReplyThatPicksNothingIsSearchedAfresh(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 30 * time.Millisecond
	heldIssue(r, "session", false)
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word false wakes"}
	r.records.hits = []dispatch.SearchHit{
		{Kind: "issue", IssueKey: "ABC-12", Title: "Wake word spotter", Score: 0.007},
		{Kind: "issue", IssueKey: "ABC-13", Title: "Wake word false wakes", Score: 0.006},
	}
	r.concierge.Heard(context.Background(), &r.exchange, "what is happening with the wake word")
	eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
	answerTurn(t, r, "what is happening with the wake word")
	r.concierge.Heard(context.Background(), &r.exchange, "the wake word timeout")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is open."))}}
	answerTurn(t, r, "the wake word timeout")
	if searches := r.records.searches(); !slices.Contains(searches, "the wake word timeout") {
		t.Fatalf("searches = %q, want the reply's own words searched", searches)
	}
}

// TestAnUnclearSearchAsksWithoutReadingTheTopHit (R5; the third review): with
// no one clear match, the voice asks which issue at once; it reads no record
// of the top hit, which it would not use.
func TestAnUnclearSearchAsksWithoutReadingTheTopHit(t *testing.T) {
	r := newRig(t)
	unclearSearch(r)
	r.records.block = make(chan struct{})
	defer close(r.records.block)
	start := time.Now()
	spoken := answerTurn(t, r, "what is happening with the realtime voice")
	if took := time.Since(start); took > time.Second || len(spoken) != 1 || !strings.HasPrefix(spoken[0].text, "Did you mean ") {
		t.Fatalf("spoken = %+v after %v, want the question which issue at once", spoken, took)
	}
	if calls := r.records.calls(); len(calls) != 0 {
		t.Fatalf("read %q, want no read of the top hit", calls)
	}
}

// TestTheHoldersQuestionKeepsItsShape (R7; the third review): the routing
// clause ends at a comma; a phrase after "how many" or an adverb after the
// question word stays with it; "whether or not" asks yes or no; a question
// whose first clause is a condition stays as said; a turn of routing alone
// keeps no stray full stop.
func TestTheHoldersQuestionKeepsItsShape(t *testing.T) {
	for turn, want := range map[string]string{
		"ask the agent on the wake word spotter, is it merged and if not, why": "Is it merged and if not, why?",
		"ask the agent for the cache, is it blocked, and who is fixing it":     "Is it blocked, and who is fixing it?",
		"how many PRs are open":                   "How many PRs are open?",
		"how much work is left":                   "How much work is left?",
		"whether or not the tap is merged":        "Is the tap merged?",
		"when exactly will it be ready":           "When exactly will it be ready?",
		"when the old cache is gone, what breaks": "When the old cache is gone, what breaks?",
		// The fourth review: with a key named, a leading clause is the
		// question's own; subjects joined by "and" or commas, and relative
		// clauses, still invert; an adjective in -ly is no adverb.
		"ask the agent on ABC-12: for the demo on Friday, will it be ready": "For the demo on Friday, will it be ready?",
		"ask the agent on ABC-12 about the tap, is it merged":               "About the tap, is it merged?",
		"whether the tap and the ring are merged":                           "Are the tap and the ring merged?",
		"whether the tap, the ring and the light are merged":                "Are the tap, the ring and the light merged?",
		"whether the agent who owns it is done":                             "Is the agent who owns it done?",
		"when the PR and the docs will land":                                "When will the PR and the docs land?",
		"when nightly builds will run again":                                "When will nightly builds run again?",
		"why daily standups are skipped":                                    "Why are daily standups skipped?",
		"when the tap ships, will the ring follow":                          "When the tap ships, will the ring follow?",
		// The fifth review: with a key, a routing clause still ends at its
		// comma; a verb inside a relative clause stays there; an -ly adverb
		// after "how", or before the verb, stays with the question word.
		"ask the agent on ABC-12 about the tap, is it merged and if not, why":      "About the tap, is it merged and if not, why?",
		"ask the agent on the wake word spotter, is ABC-12 merged and if not, why": "Is ABC-12 merged and if not, why?",
		"ask the agent on the wake word spotter, is PR 80 merged and if not, why":  "Is PR 80 merged and if not, why?",
		"ask the agent who holds ABC-12, is it merged":                             "Is it merged?",
		"whether the agent who is working on it is done":                           "Is the agent who is working on it done?",
		"when the agent who is on it will finish":                                  "When will the agent who is on it finish?",
		"how reliably does the wake word fire":                                     "How reliably does the wake word fire?",
		"when realistically will it ship":                                          "When realistically will it ship?",
		// The sixth review: a key-like number in a kept clause keeps it; a
		// pause after "agent" keeps a clause with no key; an -ly word joins
		// only the bare "how", and a name is no adverb.
		"ask the agent on ABC-12 about PR 80, is it merged":                      "About PR 80, is it merged?",
		"ask the agent on ABC-12: for the demo on November 20, will it be ready": "For the demo on November 20, will it be ready?",
		"ask the agent, for the demo on Friday, will ABC-12 be ready":            "For the demo on Friday, will ABC-12 be ready?",
		"ask the agent, on ABC-12, is it merged":                                 "Is it merged?",
		"how often nightly builds are skipped":                                   "How often are nightly builds skipped?",
		"how long early access will stay open":                                   "How long will early access stay open?",
		"when Emily will be back":                                                "When will Emily be back?",
		"why nightly is failing":                                                 "Why is nightly failing?",
		"how Kelly is doing":                                                     "How is Kelly doing?",
		// The seventh review: a clause that names the holder routes, after
		// a pause or a key alike.
		"ask the agent, who holds the issue about the wake word, is it merged": "Is it merged?",
		"ask the agent on ABC-12 who is working on the cache, is it merged":    "Is it merged?",
		"ask the agent on ABC-12.": "Ask the agent on ABC-12?",
	} {
		if got := fewestWords(turn); got != want {
			t.Errorf("fewestWords(%q) = %q, want %q", turn, got, want)
		}
	}
}

// TestHoldIsAWordOfWork (the third review): "hold" names work, as in the
// unit's "light hold"; "holds" and "holding" route.
func TestHoldIsAWordOfWork(t *testing.T) {
	if got := searchTerms("what is happening with the issue about the light hold"); got != "light hold" {
		t.Fatalf("searchTerms = %q, want light hold", got)
	}
}

// TestAFullResultInTimeBeatsTheMark (Main's rule; the third review): a full
// result that is ready when the keyword result comes, after the 2 s mark,
// still wins.
func TestAFullResultInTimeBeatsTheMark(t *testing.T) {
	for range 20 {
		r := newRig(t)
		r.concierge.fullWait = 60 * time.Millisecond
		unclearSearch(r)
		r.records.hits[0].Score = 0.012
		r.records.kwHits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Score: 0.5}, {Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.4}}
		r.records.kwDelay, r.records.searchDelay = 200*time.Millisecond, 10*time.Millisecond
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
		if spoken := answerTurn(t, r, "what is happening with the realtime voice"); len(spoken) == 0 || strings.HasPrefix(spoken[0].text, "Did you mean ") {
			t.Fatalf("spoken = %+v, want the full result, ready before the keyword one", spoken)
		}
	}
}

// TestTheFirstFactNamesTheIssue (R5 as updated): when a search chose the
// issue and the first fact is ready within 1.5 s of t0 and begins with the
// key, that fact names the issue, and "I took KEY." is not said; a first
// fact that does not begin with the key comes after "I took KEY."; with no
// fact by 1.5 s, "I took KEY." is said then, in place of the routing line.
func TestTheFirstFactNamesTheIssue(t *testing.T) {
	for _, c := range []struct {
		name  string
		fact  string
		delay time.Duration
		want  []said
	}{
		{"a fact that names the key", "ABC-12 is in review.", 0, []said{{"ABC-12 is in review.", voice.Fact}}},
		{"a fact that does not", "PR 80 is in review.", 0, []said{{"I took ABC-12.", voice.Lead}, {"PR 80 is in review.", voice.Fact}}},
		{"no fact by 1.5 s", "ABC-12 is in review.", 2200 * time.Millisecond, []said{{"I took ABC-12.", voice.Lead}, {"ABC-12 is in review.", voice.Fact}}},
	} {
		r := newRig(t)
		unclearSearch(r)
		r.records.hits[0].Score = 0.012
		r.records.issues["ABC-12"].Status = "in_review"
		if c.delay > 0 {
			delay := c.delay
			r.model.onCall = func(int) { time.Sleep(delay) }
		}
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text(c.fact))}}
		spoken := answerTurn(t, r, "what is happening with the realtime voice")
		if !slices.Equal(spoken, c.want) {
			t.Errorf("%s: spoken = %+v, want %+v", c.name, spoken, c.want)
		}
	}
}

// TestALongerKeyDoesNotNameTheIssue (R5; the lead-hold review): a first fact
// that begins with a longer key ("ABC-12" for "ABC-1") names another issue,
// so "I took ABC-1." still goes first; the chosen key followed by "'s" names
// it.
func TestALongerKeyDoesNotNameTheIssue(t *testing.T) {
	for fact, lead := range map[string]bool{"ABC-12 is merged.": true, "ABC-1's agent merged it.": false, "ABC-1 is merged.": false} {
		var spoken []said
		sp := newSpeaker(context.Background(), context.Background(), recorder(&spoken))
		if err := sp.holdLead(context.Background(), "ABC-1", "I took ABC-1."); err != nil {
			t.Fatal(err)
		}
		if err := sp.sayText(context.Background(), fact, voice.Fact); err != nil {
			t.Fatal(err)
		}
		if err := sp.wait(); err != nil {
			t.Fatal(err)
		}
		if got := len(spoken) > 0 && spoken[0].text == "I took ABC-1."; got != lead {
			t.Errorf("%q: spoken = %+v, want the line first %v", fact, spoken, lead)
		}
	}
}

// TestWhileTheHolderIsAskedTheModelSearchesNothing (R6, R7): when the
// recorder asks the holder, the model says what the record it was given
// holds, with no tool: live, it searched for what only the holder knows, and
// its first fact came after 4.8 s.
func TestWhileTheHolderIsAskedTheModelSearchesNothing(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in review."))}}
	answerTurn(t, r, "ask the agent on ABC-12 when slice one will be ready")
	if len(r.model.configs) == 0 {
		t.Fatal("no model call")
	}
	tc := r.model.configs[0].ToolConfig
	if tc == nil || tc.FunctionCallingConfig == nil || tc.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeNone {
		t.Fatalf("the model could call tools while the holder is asked: %+v", tc)
	}
}

// TestTalkOfChangeNamesNoWork (R6; run at 05ced42): "what changed on the
// issue about …" heard so far searched "changed", the read-ahead's one search
// a turn, which the final words did not hold (ahead_words=1 held=0); the
// answer searched again. Talk of change, like talk of status, names no work.
func TestTalkOfChangeNamesNoWork(t *testing.T) {
	for turn, want := range map[string]string{
		"what changed on the issue":                                         "",
		"what has changed on the issue about realtime spoken conversation?": "realtime spoken conversation",
		"any changes to the wake word spotter":                              "wake word spotter",
		"when will slice one be ready":                                      "slice",
	} {
		if got := searchTerms(turn); got != want {
			t.Errorf("searchTerms(%q) = %q, want %q", turn, got, want)
		}
	}
}

// TestTheModelIsNotOfferedWhatTheRecordHolds (R6; run at 7cc6c51): with the
// holder already read, the model is not offered issue_holder: live, it called
// it first anyway (866 ms), and the routing line played before its fact.
func TestTheModelIsNotOfferedWhatTheRecordHolds(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "none", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	answerTurn(t, r, "what is happening with ABC-12")
	if len(r.model.configs) == 0 {
		t.Fatal("no model call")
	}
	for _, tool := range r.model.configs[0].Tools {
		for _, decl := range tool.FunctionDeclarations {
			if decl.Name == "issue_holder" {
				t.Fatalf("the model was offered issue_holder with the holder in its record")
			}
		}
	}
}
