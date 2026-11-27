package concierge

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// Sol's review of #190 at 46c74247
// (https://github.com/sjawhar/ghost-wispr/pull/190#pullrequestreview-6040980666).

// TestARefreshMidTurnChangesNoDecision (the R): a turn reads the cached
// project list once, where it begins, and decides its keys, its target and
// an outage from that one reading. The list unknown when the turn began, the
// turn is an outage of Dispatch's, even when the background read the turn
// started lands, successfully, before the turn decides; it is never "I found
// no issue" for an issue that exists. The next turn reads the key from the
// list that read brought in.
func TestARefreshMidTurnChangesNoDecision(t *testing.T) {
	const turn = "what is happening with ABC-1234"
	for i := range 20 {
		r := newRig(t)
		r.records.mu.Lock()
		r.records.issues["ABC-1234"] = &dispatch.Issue{Key: "ABC-1234", Title: "Realtime voice", Status: "in_progress", UpdatedAt: testNow}
		r.records.mu.Unlock()
		// Never read: the turn's own read of the list succeeds at once.
		r.concierge.mu.Lock()
		r.concierge.projects, r.concierge.projectsRead = nil, time.Time{}
		r.concierge.mu.Unlock()
		var spoken []said
		err := r.concierge.Answer(context.Background(), &r.exchange, turn, recorder(&spoken))
		if len(spoken) != 1 || spoken[0].text != cannotReachDispatch || !errors.Is(err, voice.ErrUnreachable) {
			t.Fatalf("call %d: Answer = %v, spoken %+v; want only %q", i, err, spoken, cannotReachDispatch)
		}
		eventually(t, "the turn's read of the list landed", func() bool {
			r.concierge.mu.Lock()
			defer r.concierge.mu.Unlock()
			return r.concierge.projects != nil
		})
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-1234 is in progress."))}}
		spoken = nil
		_ = r.concierge.Answer(context.Background(), &r.exchange, turn, recorder(&spoken))
		if !slices.Contains(r.records.calls(), "ABC-1234") {
			t.Fatalf("call %d, next turn: read %q, spoken %+v; want ABC-1234 read from the list the read brought in", i, r.records.calls(), spoken)
		}
	}
}

// TestReadForDecidesFromTheTurnsView: readFor decides from the view the
// turn began with alone, whatever the cache holds by then: a view that could
// not tell is an outage though the list has since come in, and a view that
// held the project reads the key though the list has since lost it.
func TestReadForDecidesFromTheTurnsView(t *testing.T) {
	r := newRig(t)
	r.records.mu.Lock()
	r.records.issues["ABC-1234"] = &dispatch.Issue{Key: "ABC-1234", Title: "Realtime voice", Status: "in_progress", UpdatedAt: testNow}
	r.records.mu.Unlock()
	never := make(chan struct{})
	unknown := projectsView{}
	if found := r.concierge.readFor(context.Background(), &r.exchange, unknown, "what is happening with ABC-1234", nil, never); !found.unreachable {
		t.Errorf("an unknown view, the list known since: %+v, want an outage", found)
	}
	known := r.concierge.viewProjects()
	r.concierge.mu.Lock()
	r.concierge.projects, r.concierge.projectsDown = nil, true
	r.concierge.mu.Unlock()
	if found := r.concierge.readFor(context.Background(), &r.exchange, known, "what is happening with ABC-1234", nil, never); found.key != "ABC-1234" {
		t.Errorf("a view that held ABC, the list lost since: %+v, want ABC-1234 read", found)
	}
}

// TestAnOfferedKeyPicksWhenTheListCannotTell (Sol on #190): with an offer
// pending and the project list unable to tell, a key the voice offered still
// picks: that issue is real, the voice just offered it. A question, and a
// key said alone that was not offered (Main on #191), are read from the
// turn's project snapshot: a key it cannot tell with no other words to
// search is an outage of Dispatch's. Any other reply, a key among other
// words included, is side talk (Main's rule on R5's rewrite).
func TestAnOfferedKeyPicksWhenTheListCannotTell(t *testing.T) {
	for _, tc := range []struct {
		reply   string
		want    string // "pick", "outage" or "side talk"
		pending int
	}{
		{"ABC-12", "pick", 1},
		{"ABC-12, please", "pick", 1},
		{"ABC-14", "outage", 0},
		{"ABC-14, the first one", "side talk", 0},
		{"what about ABC-14", "outage", 0},
		{"what changed in the last 30 days", "outage", 0},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			got := outcomeOf(t, true, nil, tc.reply)
			switch tc.want {
			case "outage":
				if !slices.Equal(got.Spoken, []string{cannotReachDispatch}) || !got.Unreachable || len(got.Full)+len(got.Keyword) > 0 || got.Sent != 0 {
					t.Errorf("%+v; want only %q, the error sound, no search and no send", got, cannotReachDispatch)
				}
			case "side talk":
				if !got.SideTalk || len(got.Spoken) > 0 || got.Unreachable || len(got.Full)+len(got.Keyword) > 0 || got.Sent != 0 {
					t.Errorf("%+v; want side talk: nothing said, searched or sent, no error sound", got)
				}
			default:
				if got.Pending != tc.pending {
					t.Errorf("%+v; want the question first asked sent %d times", got, tc.pending)
				}
			}
		})
	}
}

// TestANewQuestionWithAnOfferPendingIsSearched (Sol's R in rounds 2 and 3 on
// #190): with an offer pending and the project list unable to tell, a new
// question whose "word NN" may be speech is searched as with no offer
// pending, and answers about the issue its search finds, an offered one
// included: by a keyword and full search, or by the full search alone, its
// meaning, when no keyword hit holds the words ("speech interface"). The
// full search is sent the question, the keyword search its words of work;
// "last 30" may be a key while the list cannot tell, so neither gets it.
func TestANewQuestionWithAnOfferPendingIsSearched(t *testing.T) {
	noKeywordHit := func(r *rig) { r.records.kwHits = nil }
	for _, tc := range []struct {
		turn, terms string
		setup       func(*rig)
	}{
		{"what changed on the realtime voice in the last 30 days", "realtime voice", nil},
		{"what changed on the realtime voice in the last 30 days?", "realtime voice", nil},
		{"what changed on the speech interface in the last 30 days", "speech interface", noKeywordHit},
		{"what changed on the speech interface in the last 30 days?", "speech interface", noKeywordHit},
	} {
		t.Run(tc.turn, func(t *testing.T) {
			got := outcomeOf(t, true, tc.setup, tc.turn)
			question := strings.Replace(strings.TrimSuffix(tc.turn, "?"), " last 30", "", 1)
			if got.Unreachable || !got.ErrNil || !slices.Equal(got.Full, []string{question}) || !slices.Equal(got.Keyword, []string{tc.terms}) || got.Took != "ABC-12" || got.Sent != 0 {
				t.Errorf("%+v; want one full search for %q and one keyword search for %q, ABC-12 answered, nothing sent", got, question, tc.terms)
			}
		})
	}
}

// TestAPendingOfferChangesNoQuestion (Main on #190, as Main's rule on R5's
// rewrite narrows it): a question that picks no offered issue is answered
// exactly as with no offer pending: the same lines, the same error, the same
// searches, the same issue and the same sends. A reply that is no question
// and no key said alone ("ABC-14 rollout", a key the list cannot tell among
// other words) is side talk with an offer pending: nothing is said, searched
// or sent.
func TestAPendingOfferChangesNoQuestion(t *testing.T) {
	noKeywordHit := func(r *rig) { r.records.kwHits = nil }
	for _, tc := range []struct {
		turn  string
		setup func(*rig)
	}{
		{"what changed on the realtime voice in the last 30 days", nil},
		{"what changed on the realtime voice in the last 30 days?", nil},
		{"what changed on the speech interface in the last 30 days", noKeywordHit},
		{"what changed on the speech interface in the last 30 days?", noKeywordHit},
		{"what is blocking ABC-14", nil},
		{"what changed in the last 30 days", nil},
		{"ABC-14 rollout?", nil},
	} {
		t.Run(tc.turn, func(t *testing.T) {
			none, pending := outcomeOf(t, false, tc.setup, tc.turn), outcomeOf(t, true, tc.setup, tc.turn)
			if !reflect.DeepEqual(none, pending) {
				t.Errorf("with no offer:  %+v\nwith an offer: %+v\nwant the same", none, pending)
			}
		})
	}
	for _, turn := range []string{"ABC-14 rollout", "ABC-14, the first one"} {
		t.Run("side talk: "+turn, func(t *testing.T) {
			got := outcomeOf(t, true, nil, turn)
			if !got.SideTalk || len(got.Spoken) > 0 || got.Unreachable || len(got.Full)+len(got.Keyword) > 0 || got.Sent != 0 || got.Took != "" {
				t.Errorf("%+v; want side talk: nothing said, searched or sent", got)
			}
		})
	}
}

// lateFullSearch is records whose full search reaches its fake only after
// delay: the goroutine searchBoth starts for the full search, run late, as the
// Go scheduler may run it.
type lateFullSearch struct {
	Records
	delay time.Duration
}

func (l lateFullSearch) Search(ctx context.Context, query string, limit int) ([]dispatch.SearchHit, error) {
	time.Sleep(l.delay)
	return l.Records.Search(ctx, query, limit)
}

// TestAnOutcomeHoldsEveryFullSearchTheAnswerStarted (Sol's review of #197 at
// 7791198, I2): searchBoth starts the full search on a goroutine of its own and
// can return the keyword result before that goroutine reaches the fake, so an
// outcome read at once may hold no full search. In a suite run
// TestAPendingOfferChangesNoNewQuestion failed so: one outcome held the full
// search and the other did not, with the same words spoken and the same
// issue taken. It fails unless outcomeOf holds every full search the answer
// started, here one that reaches the fake 200 ms late.
func TestAnOutcomeHoldsEveryFullSearchTheAnswerStarted(t *testing.T) {
	const turn = "what changed on the realtime voice in the last 30 days"
	late := func(r *rig) {
		r.concierge.mu.Lock()
		r.concierge.records = lateFullSearch{Records: r.concierge.records, delay: 200 * time.Millisecond}
		r.concierge.mu.Unlock()
	}
	onTime, late200 := outcomeOf(t, false, nil, turn), outcomeOf(t, false, late, turn)
	if len(onTime.Full) == 0 || !reflect.DeepEqual(onTime, late200) {
		t.Errorf("the full search on time: %+v\nthe full search 200 ms late: %+v\nwant the same, with the full search", onTime, late200)
	}
}

// outcome is what an answer did, as the room and Dispatch saw it.
type outcome struct {
	Spoken                []string
	Unreachable, SideTalk bool
	ErrNil                bool
	Full, Keyword         []string
	Took                  string
	Sent, Pending         int
}

// outcomeOf answers turn on a follow-up turn, the project list unable to
// tell, ABC-12 and ABC-13 offered when pending, and both searches finding
// ABC-12, "Realtime voice", unless setup changes the fakes. The read-ahead
// hears the turn first, as the Supervisor makes it. The outcome holds every
// search the answer and its read-ahead started: searchBoth starts each full
// search on a goroutine of its own beside its keyword search, and may return
// the keyword result before that goroutine reaches the fake (Sol's review of
// #197, I2), so outcomeOf waits until each keyword search has its full one.
func outcomeOf(t *testing.T, pending bool, setup func(*rig), turn string) outcome {
	t.Helper()
	r := newRig(t)
	heldIssue(r, "session", false)
	r.records.mu.Lock()
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "todo", UpdatedAt: testNow}
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
	r.records.kwHits = slices.Clone(r.records.hits)
	if setup != nil {
		setup(r)
	}
	r.records.mu.Unlock()
	projectsReadFails.set(t, r)
	r.exchange.FollowUpTurn = true
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
	if pending {
		r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.concierge.Heard(ctx, &r.exchange, turn)
	var spoken []said
	err := r.concierge.Answer(ctx, &r.exchange, turn, recorder(&spoken))
	got := outcome{Unreachable: errors.Is(err, voice.ErrUnreachable), SideTalk: errors.Is(err, voice.ErrSideTalk), ErrNil: err == nil, Took: r.exchange.IssueKey}
	for _, s := range spoken {
		got.Spoken = append(got.Spoken, s.text)
	}
	eventually(t, "every full search the answer started to reach the fake", func() bool {
		r.records.mu.Lock()
		defer r.records.mu.Unlock()
		return len(r.records.queries) >= len(r.records.kwQueries)
	})
	r.records.mu.Lock()
	got.Full, got.Keyword = slices.Clone(r.records.queries), slices.Clone(r.records.kwQueries)
	r.records.mu.Unlock()
	r.agents.mu.Lock()
	defer r.agents.mu.Unlock()
	got.Sent = len(r.agents.sent)
	for _, message := range r.agents.sent {
		if strings.Contains(message.Message, "Is the tap merged?") {
			got.Pending++
		}
	}
	return got
}
