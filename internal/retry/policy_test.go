package retry_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/retry"
)

// fakeClock is a clock that a Sleep moves forward at once, so a test waits
// out a minute's rate limit in no time.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	c.mu.Unlock()
	return ctx.Err()
}

func (c *fakeClock) total() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var sum time.Duration
	for _, d := range c.slept {
		sum += d
	}
	return sum
}

func (c *fakeClock) policy(maxRetries int, budget time.Duration) retry.Policy {
	return retry.Policy{MaxRetries: maxRetries, RateLimitBudget: budget, Sleep: c.Sleep, Now: c.Now}
}

var rateLimited = providererr.New("openai", http.StatusTooManyRequests, "rate_limit_error", "")

// TestDoWith_ARateLimitIsTriedPastMaxRetriesUntilTheBudget fails if a call
// refused 429 more often than MaxRetries allows, but for less time than the
// rate-limit budget, is given up: a limit counted by the minute refuses for
// up to a minute, whatever the number of tries.
func TestDoWith_ARateLimitIsTriedPastMaxRetriesUntilTheBudget(t *testing.T) {
	clk := newFakeClock()
	tries := 0
	err := retry.DoWith(context.Background(), func() error {
		tries++
		if tries <= 6 {
			return fmt.Errorf("openai embedding: %w", rateLimited)
		}
		return nil
	}, clk.policy(2, 90*time.Second))
	if err != nil || tries != 7 {
		t.Fatalf("DoWith = %v after %d tries; want success on the 7th", err, tries)
	}
	if total := clk.total(); total > 90*time.Second {
		t.Fatalf("waited %s, more than the 90s budget", total)
	}
}

// TestDoWith_ARateLimitStopsWithATryAtTheDeadline fails if a call refused 429
// without end is tried past the budget, is not tried once more at its end,
// or is given up with an error other than the 429.
func TestDoWith_ARateLimitStopsWithATryAtTheDeadline(t *testing.T) {
	clk := newFakeClock()
	start := clk.Now()
	var tried []time.Time
	err := retry.DoWith(context.Background(), func() error {
		tried = append(tried, clk.Now())
		return fmt.Errorf("openai embedding: %w", rateLimited)
	}, clk.policy(2, 90*time.Second))
	var answered *providererr.Error
	if !errors.As(err, &answered) || answered.Status != http.StatusTooManyRequests {
		t.Fatalf("DoWith = %v; want the 429", err)
	}
	if last := tried[len(tried)-1].Sub(start); last != 90*time.Second {
		t.Fatalf("the last try was %s after the first 429 (tries at %v); want one at the 90s deadline", last, tried)
	}
	if len(tried) < 7 {
		t.Fatalf("%d tries in 90s; want the backoff's (1, 2, 4, 8, 16, 30 s…)", len(tried))
	}
}

// TestDoWith_RetryAfterSetsTheWaitAndTheBudget fails if a 429 that names
// Retry-After is not tried again after exactly that wait, or is tried past
// it when the answer after the wait is a 429 again.
func TestDoWith_RetryAfterSetsTheWaitAndTheBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		succeedOn int
		wantTries int
		wantErr   bool
	}{
		{name: "free after the wait", succeedOn: 2, wantTries: 2},
		{name: "still limited after the wait", succeedOn: 0, wantTries: 2, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newFakeClock()
			tries := 0
			err := retry.DoWith(context.Background(), func() error {
				tries++
				if tries == tc.succeedOn {
					return nil
				}
				return retry.WithRetryAfter(rateLimited, 20*time.Second)
			}, clk.policy(5, 90*time.Second))
			if (err != nil) != tc.wantErr || tries != tc.wantTries {
				t.Fatalf("DoWith = %v after %d tries; want error %v after %d", err, tries, tc.wantErr, tc.wantTries)
			}
			if len(clk.slept) != 1 || clk.slept[0] != 20*time.Second {
				t.Fatalf("waits %v; want the one 20s Retry-After", clk.slept)
			}
		})
	}
}

// TestDoWith_OtherFailuresKeepMaxRetries fails if a policy with a rate-limit
// budget tries a failure that is not a 429 more than MaxRetries times, or a
// 429 more than that without a budget.
func TestDoWith_OtherFailuresKeepMaxRetries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		budget time.Duration
	}{
		{name: "503 with a budget", err: providererr.New("openai", http.StatusServiceUnavailable, "", ""), budget: 90 * time.Second},
		{name: "429 without a budget", err: rateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newFakeClock()
			tries := 0
			err := retry.DoWith(context.Background(), func() error {
				tries++
				return tc.err
			}, clk.policy(3, tc.budget))
			if err == nil || tries != 3 {
				t.Fatalf("DoWith = %v after %d tries; want an error after 3", err, tries)
			}
		})
	}
}

// TestDoWith_APermanentFailureIsTriedOnce fails if a 400 is tried again.
func TestDoWith_APermanentFailureIsTriedOnce(t *testing.T) {
	clk := newFakeClock()
	tries := 0
	err := retry.DoWith(context.Background(), func() error {
		tries++
		return providererr.New("openai", http.StatusBadRequest, "invalid_request_error", "")
	}, clk.policy(5, 90*time.Second))
	if err == nil || tries != 1 {
		t.Fatalf("DoWith = %v after %d tries; want an error after 1", err, tries)
	}
}

// TestDoWith_AnEndedContextEndsTheWait fails if a call is tried again once
// its context ends, or the error hides either the 429 or the end.
func TestDoWith_AnEndedContextEndsTheWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clk := newFakeClock()
	policy := clk.policy(5, 90*time.Second)
	policy.Sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	tries := 0
	err := retry.DoWith(ctx, func() error {
		tries++
		return rateLimited
	}, policy)
	var answered *providererr.Error
	if tries != 1 || !errors.Is(err, context.Canceled) || !errors.As(err, &answered) {
		t.Fatalf("DoWith = %v after %d tries; want the 429 and the cancel after 1", err, tries)
	}
}

// TestParseRetryAfter fails if a Retry-After header's seconds or date is
// misread, or a value that is neither is read as a wait.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"30", 30 * time.Second, true},
		{" 7 ", 7 * time.Second, true},
		{"0", 0, true},
		{now.Add(45 * time.Second).Format(http.TimeFormat), 45 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"", 0, false},
		{"-5", 0, false},
		{"soon", 0, false},
	} {
		got, ok := retry.ParseRetryAfter(tc.value, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseRetryAfter(%q) = %s, %v; want %s, %v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}
