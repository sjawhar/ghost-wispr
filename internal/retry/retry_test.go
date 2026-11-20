package retry_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/retry"
)

func TestDo_RetryableErrorEventuallySucceeds(t *testing.T) {
	attempts := 0
	err := retry.Do(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return errors.New("503 service unavailable")
		}
		return nil
	}, 5)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestDo_PermanentErrorNotRetried(t *testing.T) {
	attempts := 0
	err := retry.Do(context.Background(), func() error {
		attempts++
		return errors.New("401 unauthorized: invalid api key")
	}, 5)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt (no retry), got %d", attempts)
	}
}

func TestDo_MaxRetriesRespected(t *testing.T) {
	attempts := 0
	err := retry.Do(context.Background(), func() error {
		attempts++
		return errors.New("503 service unavailable")
	}, 3)
	if err == nil {
		t.Fatal("expected error after max retries")
	}
	if attempts != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", attempts)
	}
}

func TestDo_ContextCancellationStopsRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	go func() {
		// Cancel after first attempt
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := retry.Do(ctx, func() error {
		attempts++
		return fmt.Errorf("503 service unavailable")
	}, 5)
	if err == nil {
		t.Fatal("expected error after context cancellation")
	}
	if attempts > 2 {
		t.Errorf("expected at most 2 attempts after cancel, got %d", attempts)
	}
}

// A provider's error keeps no message (providererr), so its status and code
// decide: a quota refused as 403 RESOURCE_EXHAUSTED is retried, as its
// message "quota exceeded" once made it, and a 400 INVALID_ARGUMENT is not.
func TestDo_AProviderErrorIsRetriedByItsStatusAndCode(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{providererr.New("gemini", 403, "RESOURCE_EXHAUSTED", "quota exceeded"), 2},
		{providererr.New("openai", 429, "", ""), 2},
		{providererr.New("gemini", 503, "UNAVAILABLE", ""), 2},
		{providererr.New("gemini", 400, "INVALID_ARGUMENT", "try again with fewer words"), 1},
		{providererr.New("openai", 401, "invalid_api_key", ""), 1},
	} {
		attempts := 0
		_ = retry.Do(context.Background(), func() error {
			attempts++
			return fmt.Errorf("gemini json completion: %w", tc.err)
		}, 2)
		if attempts != tc.want {
			t.Errorf("%v: %d attempts, want %d", tc.err, attempts, tc.want)
		}
	}
}
