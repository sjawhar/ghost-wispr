package concierge

import (
	"context"
	"errors"
	"fmt"
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
	if found := r.concierge.readFor(context.Background(), &r.exchange, unknown, "what is happening with ABC-1234", nil, nil, never); !found.unreachable {
		t.Errorf("an unknown view, the list known since: %+v, want an outage", found)
	}
	known := r.concierge.viewProjects()
	r.concierge.mu.Lock()
	r.concierge.projects, r.concierge.projectsDown = nil, true
	r.concierge.mu.Unlock()
	if found := r.concierge.readFor(context.Background(), &r.exchange, known, "what is happening with ABC-1234", nil, nil, never); found.key != "ABC-1234" {
		t.Errorf("a view that held ABC, the list lost since: %+v, want ABC-1234 read", found)
	}
}

// TestAnOfferedKeyPicksWhenTheListCannotTell (Sol on #190, rounds 1 and 2):
// with an offer pending and the project list unable to tell, a key the voice
// offered still picks: that issue is real, the voice just offered it. Any
// other turn is read as with no offer pending: a key the list cannot tell
// with no other words to search is an outage of Dispatch's, and a reply with
// other words is searched as a new question. When it names a key the list
// cannot tell, other than the offered ones, its search never resolves to an
// offered issue whose own title holds none of the words searched: "ABC-14
// rollout" never answers about the offered ABC-12.
func TestAnOfferedKeyPicksWhenTheListCannotTell(t *testing.T) {
	for _, tc := range []struct {
		reply string
		// want is the line the voice says first, "" for the answer's facts.
		want    string
		pending int
	}{
		{"ABC-12", "", 1},
		{"ABC-12, please", "", 1},
		{"ABC-14", cannotReachDispatch, 0},
		{"ABC-14, the first one", cannotReachDispatch, 0},
		{"what changed in the last 30 days", cannotReachDispatch, 0},
		{"ABC-14 rollout", noIssueFound, 0},
		{"what is blocking ABC-14", noIssueFound, 0},
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
			switch tc.want {
			case cannotReachDispatch:
				if len(spoken) != 1 || spoken[0].text != cannotReachDispatch || !errors.Is(err, voice.ErrUnreachable) {
					t.Errorf("Answer = %v, spoken %+v; want only %q", err, spoken, cannotReachDispatch)
				}
				if searched != 0 || len(sent) != 0 {
					t.Errorf("searched %d times, sent %d; want nothing", searched, len(sent))
				}
			case noIssueFound:
				if len(spoken) != 1 || spoken[0].text != noIssueFound || err != nil {
					t.Errorf("Answer = %v, spoken %+v; want only %q", err, spoken, noIssueFound)
				}
				if searched == 0 || len(sent) != 0 || r.exchange.IssueKey != "" {
					t.Errorf("searched %d times, sent %d, took %q; want a search that takes no offered issue", searched, len(sent), r.exchange.IssueKey)
				}
			default:
				if pending != tc.pending {
					t.Errorf("sent the question first asked %d times, want %d; spoken %+v, err %v", pending, tc.pending, spoken, err)
				}
			}
		})
	}
}

// TestANewQuestionWithAnOfferPendingIsSearched (Sol's R in round 2 on #190):
// with an offer pending and the project list unable to tell, a new question
// whose "word NN" may be speech is searched as with no offer pending, and
// resolves to the issue its words describe, an offered one included.
func TestANewQuestionWithAnOfferPendingIsSearched(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, turn := range []string{
			"what changed on the realtime voice in the last 30 days",
			"what changed on the realtime voice in the last 30 days?",
		} {
			t.Run(fmt.Sprintf("offer pending %v, %s", pending, turn), func(t *testing.T) {
				r := newRig(t)
				voiceIssue(r)
				projectsReadFails.set(t, r)
				r.exchange.FollowUpTurn = true
				if pending {
					r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
				}
				r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r.concierge.Heard(ctx, &r.exchange, turn)
				var spoken []said
				err := r.concierge.Answer(ctx, &r.exchange, turn, recorder(&spoken))
				r.records.mu.Lock()
				full, keywords := slices.Clone(r.records.queries), slices.Clone(r.records.kwQueries)
				r.records.mu.Unlock()
				r.agents.mu.Lock()
				sent := len(r.agents.sent)
				r.agents.mu.Unlock()
				if errors.Is(err, voice.ErrUnreachable) || !slices.Equal(full, []string{"realtime voice"}) || !slices.Equal(keywords, []string{"realtime voice"}) || r.exchange.IssueKey != "ABC-12" || sent != 0 {
					t.Errorf("Answer = %v, full %q, keyword %q, took %q, sent %d, spoken %+v; want one search of each for \"realtime voice\", ABC-12 taken, nothing sent", err, full, keywords, r.exchange.IssueKey, sent, spoken)
				}
			})
		}
	}
}

// TestAnUntoldKeyNeverAsksAnOfferedIssuesHolder: a reply said with the wake
// phrase, so its holder may be asked, that names a key the list cannot tell
// sends no question to the holder of an offered issue its search turned up
// and its words do not describe.
func TestAnUntoldKeyNeverAsksAnOfferedIssuesHolder(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.records.mu.Lock()
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
	r.records.kwHits = slices.Clone(r.records.hits)
	r.records.mu.Unlock()
	projectsReadFails.set(t, r)
	r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
	var spoken []said
	_ = r.concierge.Answer(context.Background(), &r.exchange, "ABC-14 rollout", recorder(&spoken))
	time.Sleep(100 * time.Millisecond)
	r.agents.mu.Lock()
	defer r.agents.mu.Unlock()
	if len(r.agents.sent) != 0 || r.exchange.IssueKey != "" {
		t.Errorf("sent %+v, took %q, spoken %+v; want nothing sent and no issue taken", r.agents.sent, r.exchange.IssueKey, spoken)
	}
}

// TestWithoutAvoided: an offered issue whose title holds no word searched
// leaves a search's result; one whose title does stays. A clear match taken
// out leaves the rest unclear; with none left, no issue was found.
func TestWithoutAvoided(t *testing.T) {
	avoided := avoidedIn(twoOffered, "search:rollout")
	clear := &reading{searched: true, clear: true, key: "ABC-12", candidates: []candidate{{"ABC-12", "Realtime voice"}, {"ABC-77", "Rollout plan"}}}
	if got := withoutAvoided(clear, avoided); got.clear || got.key != "" || len(got.candidates) != 1 || got.candidates[0].key != "ABC-77" {
		t.Errorf("a clear offered match: %+v, want ABC-77 asked about", got)
	}
	only := &reading{searched: true, clear: true, key: "ABC-12", candidates: []candidate{{"ABC-12", "Realtime voice"}}}
	if got := withoutAvoided(only, avoided); !got.noResult {
		t.Errorf("only an offered match: %+v, want no issue found", got)
	}
	other := &reading{searched: true, clear: true, key: "ABC-77", candidates: []candidate{{"ABC-77", "Rollout plan"}, {"ABC-12", "Realtime voice"}}}
	if got := withoutAvoided(other, avoided); got != other {
		t.Errorf("a clear match not offered: %+v, want it as it came", got)
	}
	described := avoidedIn(twoOffered, "search:realtime voice")
	if got := withoutAvoided(only, described); got != only {
		t.Errorf("an offered match its title describes: %+v, want it as it came", got)
	}
}
