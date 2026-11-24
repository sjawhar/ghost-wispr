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

// TestAnOfferedKeyPicksWhenTheListCannotTell (the I): with an offer
// pending and the project list unable to tell, a key the voice offered still
// picks: that issue is real, the voice just offered it. Any other key the
// list cannot tell is an outage of Dispatch's, whatever else the reply
// holds, a "word NN" that may be speech included: nothing is picked,
// searched or sent.
func TestAnOfferedKeyPicksWhenTheListCannotTell(t *testing.T) {
	for _, tc := range []struct {
		reply   string
		outage  bool
		pending int
	}{
		{"ABC-12", false, 1},
		{"ABC-12, please", false, 1},
		{"ABC-14", true, 0},
		{"ABC-14 rollout", true, 0},
		{"ABC-14, the first one", true, 0},
		{"what is blocking ABC-14", true, 0},
		{"what changed in the last 30 days", true, 0},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			r := newRig(t)
			heldIssue(r, "session", false)
			r.records.mu.Lock()
			r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "todo", UpdatedAt: testNow}
			r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
			r.records.kwHits = slices.Clone(r.records.hits)
			r.records.mu.Unlock()
			projectsReadFails.set(t, r)
			r.exchange.FollowUpTurn = true
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
			r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, tc.reply, recorder(&spoken))
			r.records.mu.Lock()
			searched := len(r.records.queries) + len(r.records.kwQueries)
			r.records.mu.Unlock()
			r.agents.mu.Lock()
			sent := slices.Clone(r.agents.sent)
			r.agents.mu.Unlock()
			pending := 0
			for _, message := range sent {
				if strings.Contains(message.Message, "Is the tap merged?") {
					pending++
				}
			}
			if tc.outage {
				if len(spoken) != 1 || spoken[0].text != cannotReachDispatch || !errors.Is(err, voice.ErrUnreachable) {
					t.Errorf("Answer = %v, spoken %+v; want only %q", err, spoken, cannotReachDispatch)
				}
				if searched != 0 || len(sent) != 0 {
					t.Errorf("searched %d times, sent %d; want nothing", searched, len(sent))
				}
			} else if pending != tc.pending {
				t.Errorf("sent the question first asked %d times, want %d; spoken %+v, err %v", pending, tc.pending, spoken, err)
			}
		})
	}
}
