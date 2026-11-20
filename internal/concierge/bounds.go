package concierge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// boundedRecords bounds each Dispatch read by bound and each search by
// searchBound: a Dispatch that takes the connection and never answers must
// not spend the answer's 8 s. A read past its bound is Dispatch out of reach
// (dispatch.ErrUnreachable); a search past its own is slow, not out of reach
// (errSearchSlow).
type boundedRecords struct {
	inner       Records
	bound       time.Duration
	searchBound time.Duration
}

// errSearchSlow is a search that found nothing within its bound.
var errSearchSlow = errors.New("concierge: the search found nothing in its time")

func (b boundedRecords) within(ctx context.Context, read func(context.Context) error) error {
	return boundCall(ctx, b.bound, dispatch.ErrUnreachable, read)
}

// boundCall runs call within bound; a call the bound ended, and not its
// caller, returns past.
func boundCall(ctx context.Context, bound time.Duration, past error, call func(context.Context) error) error {
	callCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	err := call(callCtx)
	if err != nil && ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: no answer in %v", past, bound)
	}
	return err
}

func (b boundedRecords) Search(ctx context.Context, query string, limit int) (hits []dispatch.SearchHit, err error) {
	err = boundCall(ctx, b.searchBound, errSearchSlow, func(ctx context.Context) error {
		hits, err = b.inner.Search(ctx, query, limit)
		return err
	})
	return hits, err
}

func (b boundedRecords) SearchKeywords(ctx context.Context, query string, limit int) (hits []dispatch.SearchHit, err error) {
	err = boundCall(ctx, b.searchBound, errSearchSlow, func(ctx context.Context) error {
		hits, err = b.inner.SearchKeywords(ctx, query, limit)
		return err
	})
	return hits, err
}

func (b boundedRecords) Issue(ctx context.Context, key string) (issue *dispatch.Issue, err error) {
	err = b.within(ctx, func(ctx context.Context) error {
		issue, err = b.inner.Issue(ctx, key)
		return err
	})
	return issue, err
}

func (b boundedRecords) Events(ctx context.Context, key string, limit int) (events []dispatch.Event, err error) {
	err = b.within(ctx, func(ctx context.Context) error {
		events, err = b.inner.Events(ctx, key, limit)
		return err
	})
	return events, err
}

func (b boundedRecords) DocumentText(ctx context.Context, key, slug string) (text string, err error) {
	err = b.within(ctx, func(ctx context.Context) error {
		text, err = b.inner.DocumentText(ctx, key, slug)
		return err
	})
	return text, err
}

func (b boundedRecords) Projects(ctx context.Context) (keys []string, err error) {
	err = b.within(ctx, func(ctx context.Context) error {
		keys, err = b.inner.Projects(ctx)
		return err
	})
	return keys, err
}

// lookup reads session id's liveness from Envoy, bounded by c.lookupBound.
func (c *Concierge) lookup(ctx context.Context, agents Agents, id string) (envoy.SessionInfo, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, c.lookupBound)
	defer cancel()
	return agents.Lookup(lookupCtx, id)
}
