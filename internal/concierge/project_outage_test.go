package concierge

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// outageAnswer answers turn on a follow-up turn of r, ABC-13 offered and a
// clear search hit on another issue, XYZ-99, waiting: what the voice said,
// Answer's error, how many searches and sends it made, the issue it took,
// and how long it took.
type outageAnswer struct {
	spoken         []said
	err            error
	searched, sent int
	took           string
	elapsed        time.Duration
}

func answerInOutage(t *testing.T, state projectsState, turn string) outageAnswer {
	t.Helper()
	r := newRig(t)
	heldIssue(r, "session", false)
	r.records.mu.Lock()
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "todo", UpdatedAt: testNow}
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "XYZ-99", Title: "Rollout plan", Score: 0.9}}
	r.records.kwHits = slices.Clone(r.records.hits)
	r.records.mu.Unlock()
	state.set(t, r)
	r.exchange.FollowUpTurn = true
	r.concierge.offerPick(&r.exchange, "ask the agent on the wake word whether the tap is merged", []candidate{{"ABC-13", "Wake word"}})
	var got outageAnswer
	start := time.Now()
	got.err = r.concierge.Answer(context.Background(), &r.exchange, turn, recorder(&got.spoken))
	got.elapsed = time.Since(start)
	r.records.mu.Lock()
	got.searched = len(r.records.queries) + len(r.records.kwQueries)
	r.records.mu.Unlock()
	r.agents.mu.Lock()
	got.sent = len(r.agents.sent)
	r.agents.mu.Unlock()
	got.took = r.exchange.IssueKey
	return got
}

// TestAKeyTheProjectListCannotTellIsAnOutage (R10; Main on #187): when the
// check of a key's project fails because Dispatch did not answer (its list
// was never read, its read hangs or failed), the room hears that Dispatch is
// out of reach, with the error sound, as for any other outage of Dispatch's,
// and nothing is picked, searched or sent: for a key said letter by letter
// in any turn, and for a written key with no other words to search. "I
// found no issue for that" is only for a project Dispatch answered it does
// not have. No answer waits for the list.
func TestAKeyTheProjectListCannotTellIsAnOutage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state projectsState
		turn  string
	}{
		{"a spelled key as a reply, the read failed", projectsReadFails, "A B C thirteen"},
		{"a spelled key in a question, the read hangs", projectsNeverRead, "what is happening with A B C thirteen"},
		{"a spelled key, the refresh of a cached list failed", projectsRefreshFailed, "A B C thirteen"},
		{"a written key alone, the read hangs", projectsNeverRead, "what about ABC-12"},
		{"a written key alone, the read failed", projectsReadFails, "what about ABC-12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := answerInOutage(t, tc.state, tc.turn)
			if len(got.spoken) != 1 || got.spoken[0].text != cannotReachDispatch {
				t.Errorf("spoken %+v, want only %q", got.spoken, cannotReachDispatch)
			}
			if !errors.Is(got.err, voice.ErrUnreachable) {
				t.Errorf("Answer = %v, want voice.ErrUnreachable for the error sound", got.err)
			}
			if got.searched != 0 || got.sent != 0 || got.took != "" {
				t.Errorf("searched %d times, sent %d, took %q; want nothing", got.searched, got.sent, got.took)
			}
			if got.elapsed > time.Second {
				t.Errorf("the answer took %v: it waited for the project list", got.elapsed)
			}
		})
	}
	t.Run("control: a project Dispatch answered it does not have", func(t *testing.T) {
		got := answerInOutage(t, projectsKnown, "what is happening with FOO-1234")
		if errors.Is(got.err, voice.ErrUnreachable) || len(got.spoken) != 1 || got.spoken[0].text != noIssueFound {
			t.Errorf("Answer = %v, spoken %+v; want only %q and no outage", got.err, got.spoken, noIssueFound)
		}
	})
}

// TestAKeyQuestionDoesNotWaitForAStaleList (Sol's I on #187): a fresh key
// question reads its key from the cached project list at once, while the
// list's refresh runs in the background, rather than wait up to a read's
// bound for it.
func TestAKeyQuestionDoesNotWaitForAStaleList(t *testing.T) {
	got := answerInOutage(t, projectsStale, "what is happening with ABC-12")
	if got.took != "ABC-12" || errors.Is(got.err, voice.ErrUnreachable) {
		t.Errorf("took %q, Answer = %v, spoken %+v; want ABC-12 read from the cached list", got.took, got.err, got.spoken)
	}
	if got.elapsed > time.Second {
		t.Errorf("the answer took %v: it waited for the stale list's refresh", got.elapsed)
	}
}

// TestAWrittenWordNNAmongOtherWordsIsSearched: in a fresh question, with no
// offer pending, a written "word NN" the project list cannot tell may be
// speech ("the last 30 days"), so the turn's other words are searched as
// before; the outage is only for a key with nothing else to search.
func TestAWrittenWordNNAmongOtherWordsIsSearched(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	projectsReadFails.set(t, r)
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "what changed on the realtime voice in the last 30 days", recorder(&spoken))
	r.records.mu.Lock()
	searched := len(r.records.queries) + len(r.records.kwQueries)
	r.records.mu.Unlock()
	if searched == 0 || errors.Is(err, voice.ErrUnreachable) {
		t.Errorf("searched %d times, Answer = %v, spoken %+v; want the other words searched", searched, err, spoken)
	}
}
