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
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// speechProjects are projects whose keys are words of speech too: BET, a
// project of Dispatch's, and OPS (Sol's review of #194), beside INFRA and
// ABC.
var speechProjects = []string{"ABC", "INFRA", "BET", "OPS"}

// speechView is a view that holds speechProjects.
func speechView() projectsView {
	view := projectsView{known: map[string]bool{}, sure: true}
	for _, project := range speechProjects {
		view.known[project] = true
	}
	return view
}

// TestAOneDigitKeyInSpeechIsSpeech (Sol's review of #194, O1): at 0081b237
// a project and one digit were a key anywhere in a turn, so "I bet 5" read
// BET-5. A key of one digit is a key only when it is written as one
// ("INFRA-7"), or said as a bare issue reference: the turn is the key alone,
// or the key is the question's whole object ("what about infra 7"). Inside
// other speech ("I bet 5", "the infra 3 times", "ops 1 and 2") it is speech:
// no key, and its words are searched. Each turn is read as said, and as the
// Supervisor hands it over (transcript.WriteKeys). The key rows and the
// controls pass at 0081b237; each speech row fails there.
func TestAOneDigitKeyInSpeechIsSpeech(t *testing.T) {
	r := newRig(t)
	view := speechView()
	for _, tc := range []struct {
		turn string
		// keys are the keys read, "" for none; word, for a turn of speech,
		// is a word its search must keep.
		keys, word string
	}{
		{turn: "Who is working on INFRA-7?", keys: "INFRA-7"},
		{turn: "what about infra 7", keys: "INFRA-7"},
		{turn: "status of infra 7", keys: "INFRA-7"},
		{turn: "who's on infra 7", keys: "INFRA-7"},
		{turn: "infra 7", keys: "INFRA-7"},
		{turn: "What is happening with INFRA 7", keys: "INFRA-7"},
		{turn: "what about INFRA seven?", keys: "INFRA-7"},
		{turn: "Who is working on INFRA? 7.", keys: "INFRA-7"},
		{turn: "what about bet 5", keys: "BET-5"},
		{turn: "ask the agent on infra 7 is it merged", keys: "INFRA-7"},
		{turn: "I bet 5", word: "bet"},
		{turn: "I'd bet 2 of them", word: "bet"},
		{turn: "the infra 3 times", word: "infra"},
		{turn: "ops 1 and 2", word: "ops"},
		{turn: "I bet five dollars", word: "bet"},
		// Controls: no project, or a number that is no key's.
		{turn: "what changed in the last 7 days"},
		{turn: "room 2", word: "room"},
		{turn: "does it build on infra 6.8", word: "infra"},
	} {
		for _, said := range []string{tc.turn, transcript.WriteKeys(tc.turn, speechProjects)} {
			target := r.concierge.readTarget(view, said)
			if got := keysOf(target); got != tc.keys {
				t.Errorf("readTarget(%q) names %q, want %q", said, got, tc.keys)
			}
			question, terms := searchOf(target)
			if tc.word != "" && (!slices.Contains(strings.Fields(question), tc.word) || !slices.Contains(strings.Fields(terms), tc.word)) {
				t.Errorf("readTarget(%q) = %q, want a search whose question and keywords keep %q", said, target, tc.word)
			}
		}
	}
}

// TestSpeechReadsNoIssueAndAsksNoOne (Sol's review of #194, O1): Sol's
// probe, through Answer, with the issues the speech would name and their
// live holders. At 0081b237 "I bet 5" read BET-5 and asked its holder, and
// so did "I'd bet 2 of them" (BET-2), "the infra 3 times" (INFRA-3) and
// "ops 1" inside other words (OPS-1). Each turn comes as the Supervisor
// hands it over: word by word to the read-ahead (Heard), then whole to
// Answer, each time through transcript.WriteKeys. It fails unless the answer
// to each turn reads none of them, names none, says none and sends nothing,
// and its words are searched. The read-ahead reads as main's does, the words
// so far: while "ops 1" is the whole turn it is a bare reference, so the
// read-ahead reads OPS-1, and the answer to "ops 1 and 2" must not use that
// read; the other turns read nothing ahead. A control turn that names BET-5
// as the question's object reads it and asks its holder, so the probe can
// see a read and a send.
func TestSpeechReadsNoIssueAndAsksNoOne(t *testing.T) {
	speech := map[string]struct{ word, ahead string }{
		"I bet 5":           {word: "bet"},
		"I'd bet 2 of them": {word: "bet"},
		"the infra 3 times": {word: "infra"},
		"ops 1 and 2":       {word: "ops", ahead: "OPS-1"},
	}
	keys := []string{"BET-5", "BET-2", "INFRA-3", "OPS-1"}
	for turn, tc := range speech {
		r := speechRig(t)
		heardTurn(r, turn)
		ahead := r.records.calls()
		if tc.ahead == "" && len(ahead) != 0 || tc.ahead != "" && !slices.Contains(ahead, tc.ahead) {
			t.Errorf("%q read %q ahead, want %q", turn, ahead, tc.ahead)
		}
		var spoken []said
		err := r.concierge.Answer(context.Background(), &r.exchange, transcript.WriteKeys(turn, speechProjects), recorder(&spoken))
		if calls := r.records.calls()[len(ahead):]; len(calls) != 0 || r.exchange.IssueKey != "" {
			t.Errorf("%q: the answer read issues %q and named %q, want none (Answer = %v, spoken %+v)", turn, calls, r.exchange.IssueKey, err, spoken)
		}
		for _, s := range spoken {
			if slices.ContainsFunc(keys, func(key string) bool { return strings.Contains(s.text, key) }) {
				t.Errorf("%q said %q, want no key", turn, s.text)
			}
		}
		for _, request := range r.model.requests {
			if record := promptOf(request); slices.ContainsFunc(keys, func(key string) bool { return strings.Contains(record, key) }) {
				t.Errorf("%q: the model's record names a key:\n%s", turn, record)
			}
		}
		if sent := r.agents.asked(); sent != 0 {
			t.Errorf("%q sent %d questions, want none", turn, sent)
		}
		if searches := r.records.searches(); !slices.ContainsFunc(searches, func(q string) bool { return slices.Contains(strings.Fields(q), tc.word) }) {
			t.Errorf("%q searched %q, want a search that keeps %q", turn, searches, tc.word)
		}
	}
	r := speechRig(t)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("BET-5 is in progress."))}}
	var spoken []said
	if err := sayTurn(r, "what about bet 5", recorder(&spoken)); err != nil {
		t.Fatalf("control: Answer = %v, spoken %+v", err, spoken)
	}
	if calls := r.records.calls(); !slices.Contains(calls, "BET-5") || r.agents.asked() != 1 {
		t.Fatalf("control: %q read %q and sent %d questions; want BET-5 read and its holder asked", "what about bet 5", calls, r.agents.asked())
	}
}

// heardTurn says turn to the read-ahead as the Supervisor hands it over, the
// keys of speechProjects written (transcript.WriteKeys): word by word, each
// word 50 ms after the last, and AskerTurn the words so far.
func heardTurn(r *rig, turn string) {
	words := strings.Fields(turn)
	for i := range words {
		r.exchange.AskerTurn = transcript.WriteKeys(strings.Join(words[:i+1], " "), speechProjects)
		r.concierge.Heard(context.Background(), &r.exchange, r.exchange.AskerTurn)
		time.Sleep(50 * time.Millisecond)
	}
}

// sayTurn says turn as the Supervisor hands it over (heardTurn), then whole
// to Answer.
func sayTurn(r *rig, turn string, say voice.SayFunc) error {
	heardTurn(r, turn)
	return r.concierge.Answer(context.Background(), &r.exchange, transcript.WriteKeys(turn, speechProjects), say)
}

// TestAOneDigitKeyAtTheEndStopsTheSearchOfOlderWords: "what about infra"
// settles into a search of "infra"; "7" makes it "what about infra 7", a
// key, so the read-ahead reads INFRA-7, and the search of the older words is
// stale: it stops before it reaches Dispatch, as for any new target. The
// answer reads INFRA-7 and searches nothing. It fails if the older search
// reaches Dispatch.
func TestAOneDigitKeyAtTheEndStopsTheSearchOfOlderWords(t *testing.T) {
	r := speechRig(t)
	r.records.issues["INFRA-7"] = &dispatch.Issue{Key: "INFRA-7", Title: "Realtime voice", Status: "todo", UpdatedAt: testNow.Add(-time.Hour)}
	r.concierge.Heard(context.Background(), &r.exchange, "what about infra")
	time.Sleep(50 * time.Millisecond)
	r.concierge.Heard(context.Background(), &r.exchange, "what about infra 7")
	time.Sleep(2 * r.concierge.searchSettle)
	searched := func() []string {
		r.records.mu.Lock()
		defer r.records.mu.Unlock()
		return append(slices.Clone(r.records.queries), r.records.kwQueries...)
	}
	if queries := searched(); len(queries) != 0 {
		t.Fatalf("the read-ahead searched %q after the turn named INFRA-7 at its end, want no search", queries)
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-7 is todo."))}}
	answerTurn(t, r, "what about infra 7")
	if calls, queries := r.records.calls(), searched(); !slices.Contains(calls, "INFRA-7") || len(queries) != 0 {
		t.Fatalf("the answer read %q and searched %q; want INFRA-7 read and no search", calls, queries)
	}
}

// asked is how many sends were asked of f, the failed ones too.
func (f *fakeAgents) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attempts)
}

// speechRig is a rig whose project list holds speechProjects, with the issues
// Sol's probe named, each held by a live agent session.
func speechRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.concierge.mu.Lock()
	r.concierge.projects = speechView().known
	r.concierge.mu.Unlock()
	for i, key := range []string{"BET-5", "BET-2", "INFRA-3", "OPS-1"} {
		session := "agent-" + strings.ToLower(key)
		r.records.issues[key] = &dispatch.Issue{
			Key: key, Title: "Issue " + key, Status: "in_progress",
			UpdatedAt: testNow.Add(-time.Duration(i+1) * time.Minute),
			Claim:     &dispatch.Claim{Actor: actor("session", session, "Worker on "+key)},
		}
		r.records.events[key] = []dispatch.Event{{Type: "comment.created", CreatedAt: testNow.Add(-5 * time.Minute)}}
		r.agents.states[session] = envoy.SessionInfo{SessionID: session, State: envoy.StateLive}
	}
	return r
}
