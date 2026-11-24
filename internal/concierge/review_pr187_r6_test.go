package concierge

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// Sol's sixth review of #187 at c22c6498
// (https://github.com/sjawhar/ghost-wispr/pull/187#pullrequestreview-6038975281):
// the pick fails safe and stays narrow.

// TestReview187R6AQuestionVerbAnywhereIsAQuestion (the C/R): a question's
// verb anywhere in a reply makes it a new question, after a cue or a polite
// opener too, whatever the titles say; "let's do the first one" is a new
// question by design; the framing words that stay still frame a pick.
func TestReview187R6AQuestionVerbAnywhereIsAQuestion(t *testing.T) {
	titled := []candidate{{"ABC-12", "Is the voice ready"}, {"ABC-13", "Voice has a PR"}}
	for _, tc := range []struct {
		candidates []candidate
		reply, key string
		ok         bool
	}{
		// Sol's four, with one and with two offered.
		{twoOffered, "the first one, do I pick that", "", false},
		{twoOffered, "the first one, do I go with that", "", false},
		{twoOffered, "please, do I pick the first one", "", false},
		{twoOffered, "actually, do I pick the first one", "", false},
		{twoOffered[:1], "the first one, do I pick that", "", false},
		{twoOffered[:1], "please, do I pick the first one", "", false},
		// Sol's title cases: the gate, not the titles, rejects them.
		{titled, "the first one is it", "", false},
		{titled, "the first one has a PR right", "", false},
		{[]candidate{{"ABC-12", "The voice can wait"}, {"ABC-13", "Wake word"}}, "the first one can wait", "", false},
		// New questions by design.
		{twoOffered, "let's do the first one", "", false},
		{twoOffered, "go for the first one", "", false},
		{twoOffered, "I'd say the wake word one", "", false},
		{twoOffered, "lets do ABC-13", "", false},
		// Framing words that stay.
		{twoOffered, "I think the first one", "ABC-12", true},
		{twoOffered, "the first one then", "ABC-12", true},
		{twoOffered, "I guess the wake word one", "ABC-13", true},
		{twoOffered, "probably the second one", "ABC-13", true},
		{twoOffered, "maybe the first one", "ABC-12", true},
		{twoOffered, "actually, the second one", "ABC-13", true},
		{twoOffered, "just the first one", "ABC-12", true},
		{twoOffered, "the second one now", "ABC-13", true},
	} {
		r := newRig(t)
		r.concierge.offerPick(&r.exchange, twoOfferedTurn, tc.candidates)
		_, key, ok := r.concierge.takePick(&r.exchange, tc.reply)
		if key != tc.key || ok != tc.ok {
			t.Errorf("takePick(%q) of %d offered = %q, %v; want %q, %v", tc.reply, len(tc.candidates), key, ok, tc.key, tc.ok)
		}
	}
}

// TestReview187R6QuestionsSendNothing: through Answer on a follow-up turn,
// Sol's first and third replies send nothing; the framed picks send the
// question first asked once.
func TestReview187R6QuestionsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		candidates []candidate
		reply      string
		want       int
	}{
		{twoOffered, "the first one, do I pick that", 0},
		{twoOffered, "please, do I pick the first one", 0},
		{twoOffered[:1], "the first one, do I pick that", 0},
		{twoOffered[:1], "please, do I pick the first one", 0},
		{twoOffered, "I think the first one", 1},
		{twoOffered, "the first one then", 1},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			if got := pendingSends(t, tc.candidates, tc.reply); got != tc.want {
				t.Errorf("sent the question first asked %d times, want %d", got, tc.want)
			}
		})
	}
}

// projectsState sets what r's Concierge knows of Dispatch's projects.
type projectsState int

const (
	// projectsReadFails: never read, and the read fails (503).
	projectsReadFails projectsState = iota
	// projectsNeverRead: never read, and the read hangs.
	projectsNeverRead
	// projectsRefreshFailed: a list is cached, and its last refresh failed.
	projectsRefreshFailed
	// projectsStale: a list is cached and stale, and the refresh hangs.
	projectsStale
)

// warmProjects waits for r's Concierge to know Dispatch's projects, as the
// read New starts brings them in.
func warmProjects(r *rig) {
	r.concierge.isProject(context.Background(), "ABC", true)
}

// readKeys is words as the Concierge reads a turn where it enters (answer,
// Heard), its project list known: a key said letter by letter written.
func readKeys(t *testing.T, r *rig, words string) string {
	t.Helper()
	warmProjects(r)
	read, unread := r.concierge.readSpelledKeys(context.Background(), words)
	if unread {
		t.Fatalf("readSpelledKeys(%q): the project list could not tell", words)
	}
	return read
}

func (s projectsState) set(t *testing.T, r *rig) {
	t.Helper()
	warmProjects(r) // the read New starts ends before the state is set
	r.records.mu.Lock()
	switch s {
	case projectsReadFails:
		r.records.projects = nil
	case projectsNeverRead, projectsStale:
		block := make(chan struct{})
		t.Cleanup(func() { close(block) })
		r.records.projectsBlock = block
	}
	r.records.mu.Unlock()
	r.concierge.mu.Lock()
	switch s {
	case projectsReadFails, projectsNeverRead:
		r.concierge.projects, r.concierge.projectsRead = nil, time.Time{}
	case projectsRefreshFailed:
		r.concierge.projects, r.concierge.projectsDown = map[string]bool{"ABC": true}, true
	case projectsStale:
		r.concierge.projects, r.concierge.projectsRead = map[string]bool{"ABC": true}, time.Time{}
	}
	r.concierge.mu.Unlock()
}

// TestReview187R6ASpelledKeyFailsClosed (the O): letters said one by one
// read as a key only from the cached project list, and answer never waits
// for a read of it. When the list cannot tell (never read, or its read
// failed), a run of letters that could be a key is neither rewritten nor
// searched as words: a reply picks nothing, and the voice asks for the key.
// A stale list that holds the project still reads it, at once.
func TestReview187R6ASpelledKeyFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state projectsState
		reply string
		want  string // the first line spoken
	}{
		{"a Records failure, a reply", projectsReadFails, "A B C thirteen", noIssueFound},
		{"a Records failure, a question", projectsReadFails, "what is happening with A B C thirteen", noIssueFound},
		{"an empty cache, a reply", projectsNeverRead, "A B C thirteen", noIssueFound},
		{"an empty cache, a question", projectsNeverRead, "what is happening with A B C thirteen", noIssueFound},
		{"a failed refresh of a cached list", projectsRefreshFailed, "A B C thirteen", noIssueFound},
		{"control: a stale list that holds the project", projectsStale, "A B C thirteen", "Reading ABC-13."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			heldIssue(r, "session", false)
			r.records.mu.Lock()
			r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "todo", UpdatedAt: testNow}
			r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
			r.records.kwHits = slices.Clone(r.records.hits)
			r.records.mu.Unlock()
			tc.state.set(t, r)
			r.exchange.FollowUpTurn = true
			r.concierge.offerPick(&r.exchange, "ask the agent on the wake word whether the tap is merged", []candidate{{"ABC-13", "Wake word"}})
			start := time.Now()
			var spoken []said
			_ = r.concierge.Answer(context.Background(), &r.exchange, tc.reply, recorder(&spoken))
			took := time.Since(start)
			r.records.mu.Lock()
			searched := len(r.records.queries) + len(r.records.kwQueries)
			r.records.mu.Unlock()
			r.agents.mu.Lock()
			sent := len(r.agents.sent)
			r.agents.mu.Unlock()
			if tc.want == noIssueFound {
				if len(spoken) != 1 || spoken[0].text != noIssueFound {
					t.Errorf("spoken %+v, want only %q", spoken, noIssueFound)
				}
				if searched != 0 || sent != 0 || r.exchange.IssueKey != "" {
					t.Errorf("searched %d times, sent %d, took %q; want nothing", searched, sent, r.exchange.IssueKey)
				}
			} else if r.exchange.IssueKey != "ABC-13" || searched != 0 {
				t.Errorf("took %q after %d searches; want ABC-13 and no search", r.exchange.IssueKey, searched)
			}
			if took > time.Second {
				t.Errorf("the answer took %v: it waited for the project list", took)
			}
		})
	}
}

// TestReview187R6HeardSearchesNoUnreadKey: the read-ahead neither reads a
// run of letters that could be a key as words nor searches it while the
// project list cannot tell.
func TestReview187R6HeardSearchesNoUnreadKey(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 20 * time.Millisecond
	projectsReadFails.set(t, r)
	r.concierge.Heard(context.Background(), &r.exchange, "what is happening with A B C thirteen")
	time.Sleep(300 * time.Millisecond)
	r.records.mu.Lock()
	defer r.records.mu.Unlock()
	if len(r.records.queries)+len(r.records.kwQueries) != 0 {
		t.Errorf("the read-ahead searched %q and %q; want nothing", r.records.queries, r.records.kwQueries)
	}
}
