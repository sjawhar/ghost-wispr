package retry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// DefaultMaxRetries is the default number of retry attempts.
const DefaultMaxRetries = 5

// newBackOff is the backoff between tries: 1 s, doubling to at most 30 s,
// each ±25%.
func newBackOff() *backoff.ExponentialBackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 1 * time.Second
	bo.MaxInterval = 30 * time.Second
	bo.Multiplier = 2.0
	bo.RandomizationFactor = 0.25
	return bo
}

// Do executes fn with exponential backoff retry for transient errors.
// Permanent errors (4xx except 429) are not retried.
// Retryable errors (429, 5xx, timeouts) are retried up to maxRetries times.
func Do(ctx context.Context, fn func() error, maxRetries int) error {
	return DoWith(ctx, fn, Policy{MaxRetries: maxRetries})
}

// DefaultRateLimitBudget is how long a call refused for a rate limit is
// tried again when the refusal names no Retry-After: a limit counted by the
// minute, such as a model gateway's on all its callers' embedding inputs
// together, refuses for up to a minute, and the budget is half as long
// again.
const DefaultRateLimitBudget = 90 * time.Second

// Policy is how DoWith tries a call again.
type Policy struct {
	// MaxRetries is how many tries a call gets for failures that are worth
	// retrying; DefaultMaxRetries when 0.
	MaxRetries int
	// RateLimitBudget, when set, keeps a call refused for a rate limit
	// (HTTP 429) trying past MaxRetries, until a try at a deadline the
	// first refusal sets: its Retry-After when it names one (WithRetryAfter),
	// else RateLimitBudget from it. The wait before each try after a
	// refusal is its Retry-After, else the backoff's, and never ends past
	// the deadline. Zero tries a 429 as any other retryable failure.
	RateLimitBudget time.Duration
	// Sleep waits d, or until ctx ends, and returns ctx's error then; a
	// timer when nil. Now is the clock; time.Now when nil.
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
}

// DoWith executes fn by policy: a permanent failure is tried once, any
// other up to MaxRetries times with the backoff between, and a rate limit
// past that while the budget lasts. A call whose wait is cut short by ctx
// returns its last failure and ctx's.
func DoWith(ctx context.Context, fn func() error, policy Policy) error {
	maxRetries := policy.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	sleep, now := policy.Sleep, policy.Now
	if sleep == nil {
		sleep = sleepTimer
	}
	if now == nil {
		now = time.Now
	}
	bo := newBackOff()
	var deadline time.Time // set by the first 429 a budget covers
	failures := 0
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if isPermanent(err) {
			return err
		}
		wait := bo.NextBackOff()
		if policy.RateLimitBudget > 0 && rateLimited(err) {
			after, named := retryAfter(err)
			if named {
				wait = after
			}
			if deadline.IsZero() {
				budget := policy.RateLimitBudget
				if named {
					budget = after
				}
				deadline = now().Add(budget)
			}
			left := deadline.Sub(now())
			if left <= 0 {
				return err
			}
			wait = min(wait, left)
		} else {
			failures++
			if failures >= maxRetries {
				return err
			}
		}
		if waitErr := sleep(ctx, wait); waitErr != nil {
			return fmt.Errorf("%w; stopped waiting to try again: %w", err, waitErr)
		}
	}
}

// rateLimited says whether err is a provider's answer of HTTP 429.
func rateLimited(err error) bool {
	var answered *providererr.Error
	return errors.As(err, &answered) && answered.Status == http.StatusTooManyRequests
}

// retryAfter is the wait err's answer named (WithRetryAfter).
func retryAfter(err error) (time.Duration, bool) {
	var after *AfterError
	if errors.As(err, &after) {
		return after.After, true
	}
	return 0, false
}

// sleepTimer waits d, or until ctx ends.
func sleepTimer(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// AfterError is a failure whose answer said how long to wait before trying
// again: an HTTP Retry-After.
type AfterError struct {
	Err   error
	After time.Duration
}

func (e *AfterError) Error() string { return e.Err.Error() }
func (e *AfterError) Unwrap() error { return e.Err }

// WithRetryAfter returns err with the wait its answer named.
func WithRetryAfter(err error, after time.Duration) error {
	return &AfterError{Err: err, After: after}
}

// ParseRetryAfter reads an HTTP Retry-After header: seconds, or a date (a
// date already past is no wait). ok is false for any other value.
func ParseRetryAfter(value string, now time.Time) (after time.Duration, ok bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(at.Sub(now), 0), true
}

// isPermanent returns true for errors that should NOT be retried.
// These are client errors (4xx except 429) that won't succeed on retry. A
// provider's error (providererr) keeps no message, so its status and code
// decide.
func isPermanent(err error) bool {
	if err == nil {
		return false
	}
	var answered *providererr.Error
	if errors.As(err, &answered) {
		return !answered.Retryable()
	}
	msg := strings.ToLower(err.Error())

	// Explicit retryable signals — never treat as permanent
	retryableSignals := []string{
		"429", "rate limit", "quota",
		"503", "502", "504",
		"service unavailable", "bad gateway", "gateway timeout",
		"timeout", "deadline exceeded", "connection refused",
		"temporary", "try again",
	}
	for _, s := range retryableSignals {
		if strings.Contains(msg, s) {
			return false
		}
	}

	// Permanent client errors
	permanentSignals := []string{
		"401", "403", "invalid api key", "unauthorized", "forbidden",
		"400", "bad request", "invalid argument",
	}
	for _, s := range permanentSignals {
		if strings.Contains(msg, s) {
			return true
		}
	}

	// Default: retryable (transient network errors, unknown errors)
	return false
}
