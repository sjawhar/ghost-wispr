package retry_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
		for attempts == 0 {
		}
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
