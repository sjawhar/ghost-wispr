package concierge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestOnlyTheFullSearchSlowStillSaysSlow (R6, R10): the keyword search
// answered with nothing and the full search ran past its bound. The search
// found nothing in its time; Dispatch did not answer that it has no such
// issue. It fails on main (c57adc3), which says "I found no issue for that.
// What is its key?", unless the voice says that the search is slow, with no
// error.
func TestOnlyTheFullSearchSlowStillSaysSlow(t *testing.T) {
	r := newRig(t)
	r.concierge.searchBound = 200 * time.Millisecond
	r.records.kwHits = nil
	r.records.hits = []dispatch.SearchHit{}
	r.records.searchDelay = time.Hour
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
	if err != nil || len(spoken) == 0 || spoken[len(spoken)-1].text != searchIsSlow {
		t.Fatalf("Answer = %v, spoken %+v; want %q, no error", err, spoken, searchIsSlow)
	}
}

// TestADispatchThatFailsASearchIsAnOutage (R10): a search Dispatch refused,
// not one that ran past its bound, is Dispatch out of reach, with the error
// sound: the keyword search refused (503) with the full search slow, and
// the full search refused with keyword hits whose titles hold no word
// searched. It fails on main, which says "I found no issue for that. What
// is its key?" with no error in both.
func TestADispatchThatFailsASearchIsAnOutage(t *testing.T) {
	refused := &dispatch.APIError{Status: 503, Code: "UNAVAILABLE", Route: "search"}
	for _, tc := range []struct {
		name  string
		setup func(r *rig)
	}{
		{"the keyword search refused, the full search slow", func(r *rig) {
			r.records.kwErr = refused
			r.records.searchDelay = time.Hour
		}},
		{"the full search refused, no keyword title names the words", func(r *rig) {
			r.records.kwHits = []dispatch.SearchHit{
				{Kind: "document", IssueKey: "ABC-7", Title: "Team state file", Score: 0.0102},
				{Kind: "issue", IssueKey: "ABC-9", Title: "Roadmap: one grader", Score: 0.0101},
			}
			r.records.searchErr = refused
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.concierge.searchBound = 200 * time.Millisecond
			tc.setup(r)
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
			if !errors.Is(err, voice.ErrUnreachable) || len(spoken) == 0 || spoken[len(spoken)-1].text != cannotReachDispatch {
				t.Fatalf("Answer = %v, spoken %+v; want %q and voice.ErrUnreachable", err, spoken, cannotReachDispatch)
			}
		})
	}
}

// TestAModelsSlowSearchIsToldSo (R6, R10): a search the model asks for that
// finds nothing in its time tells the model that Dispatch's search is slow,
// so the model says what is true. It fails on main, whose answer says only
// that the search took too long.
func TestAModelsSlowSearchIsToldSo(t *testing.T) {
	r := newRig(t)
	r.concierge.searchBound = 200 * time.Millisecond
	r.records.kwDelay, r.records.searchDelay = time.Hour, time.Hour
	result := r.run(t, "search_issues", map[string]string{"query": "realtime voice"})
	if note, _ := result["note"].(string); !strings.HasPrefix(note, searchIsSlow) || result["error"] != nil {
		t.Fatalf("search_issues = %v; want no error and a note that begins %q", result, searchIsSlow)
	}
}

// TestTheKnownLineAfterASlowSearchSaysSo (live on 2026-11-25): "What changed
// in the last 7 days?" names no issue; the model searched, the search ran
// past its bound, and at the 6 s mark the voice said "I found no issue for
// that. What is its key?". It fails on main, which says that line with the
// error sound, unless the voice says that the search is slow, with no
// error.
func TestTheKnownLineAfterASlowSearchSaysSo(t *testing.T) {
	r := newRig(t)
	r.concierge.searchBound, r.concierge.knownBound = 100*time.Millisecond, 600*time.Millisecond
	r.records.kwDelay, r.records.searchDelay = time.Hour, time.Hour
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(call("s1", "search_issues", map[string]any{"query": "recent changes"}, ""))}}
	block := make(chan struct{})
	defer close(block)
	r.model.onCall = func(call int) {
		if call == 1 {
			r.model.block = block // the model's next round never answers
		}
	}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "what changed in the last 7 days", recorder(&spoken))
	if err != nil || len(spoken) != 1 || spoken[0].text != searchIsSlow {
		t.Fatalf("Answer = %v, spoken %+v; want only %q, no error", err, spoken, searchIsSlow)
	}
}
