package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// fakeEnvoySession is an Envoy session that records its close.
type fakeEnvoySession struct {
	concierge.Agents
	mu     sync.Mutex
	closed bool
}

func (s *fakeEnvoySession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// TestTheEnvoySessionIsRetriedUntilItOpens fails if the unit gives up on, or
// waits a minute between, attempts to open its Envoy session when Envoy refuses
// the first connects (the Pi cannot reach Envoy until its tailnet grant
// applies, and a restart can race a network blip), or if the session it then
// opens is not used and closed at stop.
func TestTheEnvoySessionIsRetriedUntilItOpens(t *testing.T) {
	session := &fakeEnvoySession{}
	var mu sync.Mutex
	attempts := 0
	opened := make(chan struct{})
	original, originalFirst := openEnvoySession, envoyRetryFirst
	t.Cleanup(func() { openEnvoySession, envoyRetryFirst = original, originalFirst })
	envoyRetryFirst = time.Millisecond
	openEnvoySession = func(context.Context, *envoy.Listener, *envoy.SessionConfig) (envoySession, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts <= 3 {
			return nil, errors.New("connection refused")
		}
		close(opened)
		return session, nil
	}

	cfg := geminiKeyConfig("test")
	cfg.Concierge.EnvoyURL = "http://127.0.0.1:1"
	_, stop, err := startConcierge(t.Context(), &cfg, conciergeStore(t), discardLogger, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("%d attempts in 2 s, want the session open on the fourth", attempts)
	}
	stop()
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.closed {
		t.Error("stop did not close the Envoy session")
	}
}

// TestEnvoyRetriesBackOffToAMinute fails if the waits between attempts do not
// double from the first wait up to a minute, and stay there.
func TestEnvoyRetriesBackOffToAMinute(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	wait := time.Second
	for i, w := range want {
		if wait != w {
			t.Fatalf("wait %d = %v, want %v", i, wait, w)
		}
		wait = nextEnvoyRetry(wait)
	}
}
