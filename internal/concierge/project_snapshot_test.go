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
// picks: that issue is real, the voice just offered it. Any other turn is
// read exactly as with no offer pending: a key the list cannot tell with no
// other words to search is an outage of Dispatch's.
func TestAnOfferedKeyPicksWhenTheListCannotTell(t *testing.T) {
	for _, tc := range []struct {
		reply   string
		outage  bool
		pending int
	}{
		{"ABC-12", false, 1},
		{"ABC-12, please", false, 1},
		{"ABC-14", true, 0},
		{"ABC-14, the first one", true, 0},
		{"what changed in the last 30 days", true, 0},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			got := outcomeOf(t, true, nil, tc.reply)
			if tc.outage {
				if !slices.Equal(got.Spoken, []string{cannotReachDispatch}) || !got.Unreachable || len(got.Full)+len(got.Keyword) > 0 || got.Sent != 0 {
					t.Errorf("%+v; want only %q, the error sound, no search and no send", got, cannotReachDispatch)
				}
			} else if got.Pending != tc.pending {
				t.Errorf("%+v; want the question first asked sent %d times", got, tc.pending)
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

// TestAPendingOfferChangesNoNewQuestion (Main on #190): a turn that picks
// no offered issue is answered exactly as with no offer pending: the same
// lines, the same error, the same searches, the same issue and the same
// sends. "ABC-14 rollout", a key the list cannot tell among other words, is
// searched and answers about what the search finds, as on main.
func TestAPendingOfferChangesNoNewQuestion(t *testing.T) {
	noKeywordHit := func(r *rig) { r.records.kwHits = nil }
	for _, tc := range []struct {
		turn  string
		setup func(*rig)
	}{
		{"what changed on the realtime voice in the last 30 days", nil},
		{"what changed on the realtime voice in the last 30 days?", nil},
		{"what changed on the speech interface in the last 30 days", noKeywordHit},
		{"what changed on the speech interface in the last 30 days?", noKeywordHit},
		{"ABC-14 rollout", nil},
		{"what is blocking ABC-14", nil},
		{"ABC-14", nil},
		{"ABC-14, the first one", nil},
		{"what changed in the last 30 days", nil},
	} {
		t.Run(tc.turn, func(t *testing.T) {
			none, pending := outcomeOf(t, false, tc.setup, tc.turn), outcomeOf(t, true, tc.setup, tc.turn)
			if !reflect.DeepEqual(none, pending) {
				t.Errorf("with no offer:  %+v\nwith an offer: %+v\nwant the same", none, pending)
			}
		})
	}
}

// outcome is what an answer did, as the room and Dispatch saw it.
type outcome struct {
	Spoken        []string
	Unreachable   bool
	ErrNil        bool
	Full, Keyword []string
	Took          string
	Sent, Pending int
}

// outcomeOf answers turn on a follow-up turn, the project list unable to
// tell, ABC-12 and ABC-13 offered when pending, and both searches finding
// ABC-12, "Realtime voice", unless setup changes the fakes. The read-ahead
// hears the turn first, as the Supervisor makes it.
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
	got := outcome{Unreachable: errors.Is(err, voice.ErrUnreachable), ErrNil: err == nil, Took: r.exchange.IssueKey}
	for _, s := range spoken {
		got.Spoken = append(got.Spoken, s.text)
	}
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
