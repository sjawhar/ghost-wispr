package retry

import (
	"context"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// DefaultMaxRetries is the default number of retry attempts.
const DefaultMaxRetries = 5

// Do executes fn with exponential backoff retry for transient errors.
// Permanent errors (4xx except 429) are not retried.
// Retryable errors (429, 5xx, timeouts) are retried up to maxRetries times.
func Do(ctx context.Context, fn func() error, maxRetries int) error {
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 1 * time.Second
	bo.MaxInterval = 30 * time.Second
	bo.Multiplier = 2.0
	bo.RandomizationFactor = 0.25

	operation := func() (struct{}, error) {
		err := fn()
		if err == nil {
			return struct{}{}, nil
		}
		if isPermanent(err) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}

	_, err := backoff.Retry(ctx, operation,
		backoff.WithBackOff(bo),
		backoff.WithMaxTries(uint(maxRetries)),
	)
	return err
}

// isPermanent returns true for errors that should NOT be retried.
// These are client errors (4xx except 429) that won't succeed on retry.
func isPermanent(err error) bool {
	if err == nil {
		return false
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
