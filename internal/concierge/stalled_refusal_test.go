package concierge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// stallingRefusal is Dispatch as a 503 whose body stalls: its full search
// answers only when the call's context ends, and then with the refusal its
// status already said.
type stallingRefusal struct{ *fakeRecords }

func (s stallingRefusal) Search(ctx context.Context, query string, _ int) ([]dispatch.SearchHit, error) {
	s.mu.Lock()
	s.queries = append(s.queries, query)
	s.mu.Unlock()
	<-ctx.Done()
	return nil, &dispatch.APIError{Status: 503, Code: "UNAVAILABLE", Route: "search"}
}

// TestARefusalAtTheBoundIsAnOutage (#194's rule, taken on #191 by Main's
// call): a Dispatch that refused the full search, its status seen and its
// body stalled to the search's bound, refused it; the bound does not make it
// a slow search. With the keyword search finding nothing, the room hears
// that Dispatch is out of reach, with the error sound. At 01710c3 the bound
// made it slow: "Dispatch's search is slow right now.", no sound. Control:
// a search that answered nothing by its bound is still slow.
func TestARefusalAtTheBoundIsAnOutage(t *testing.T) {
	stalled := func(ctx context.Context) error {
		<-ctx.Done()
		return &dispatch.APIError{Status: 503, Route: "search"}
	}
	var refused *dispatch.APIError
	if err := boundCall(context.Background(), 50*time.Millisecond, errSearchSlow, stalled); !errors.As(err, &refused) || errors.Is(err, errSearchSlow) {
		t.Errorf("a refusal at the bound = %v, want the 503", err)
	}
	silent := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := boundCall(context.Background(), 50*time.Millisecond, errSearchSlow, silent); !errors.Is(err, errSearchSlow) {
		t.Errorf("control: no answer by the bound = %v, want a slow search", err)
	}

	r := newRig(t)
	r.concierge.searchBound = 200 * time.Millisecond
	r.records.kwHits = nil
	r.concierge.records = stallingRefusal{r.records}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
	if !errors.Is(err, voice.ErrUnreachable) || len(spoken) == 0 || spoken[len(spoken)-1].text != cannotReachDispatch {
		t.Errorf("Answer = %v, spoken %+v; want %q and voice.ErrUnreachable", err, spoken, cannotReachDispatch)
	}
}
