package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
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

	cfg := config.Config{GenAIBackend: "gemini", GeminiAPIKey: "test", Concierge: config.Concierge{EnvoyURL: "http://127.0.0.1:1"}}
	_, stop, err := startConcierge(t.Context(), &cfg, conciergeStore(t), discardLogger, nil, nil)
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
	stop(context.Background())
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.closed {
		t.Error("stop did not close the Envoy session")
	}
}

// TestAStopWaitsForTheEnvoySessionsOpeningOnlyWithinItsDeadline fails if the
// Concierge's stop waits past its context's end for an Envoy session's
// opening that does not see the cancel, as a resume point's store write
// waiting for the store's one connection does not: the unit's shutdown would
// hang there and never close its store. It also fails if a session that
// opening completes after the stop gave up is never closed, which would leave
// its registration at Envoy. The control is
// TestTheEnvoySessionIsRetriedUntilItOpens, where the opening ends before the
// stop and the stop closes the session it opened.
func TestAStopWaitsForTheEnvoySessionsOpeningOnlyWithinItsDeadline(t *testing.T) {
	opening, release := make(chan struct{}), make(chan struct{})
	late := &fakeEnvoySession{}
	original := openEnvoySession
	t.Cleanup(func() { openEnvoySession = original })
	openEnvoySession = func(context.Context, *envoy.Listener, *envoy.SessionConfig) (envoySession, error) {
		close(opening)
		<-release // it does not see the cancel, and then opens the session
		return late, nil
	}
	cfg := config.Config{GenAIBackend: "gemini", GeminiAPIKey: "test", Concierge: config.Concierge{EnvoyURL: "http://127.0.0.1:1"}}
	_, stop, err := startConcierge(t.Context(), &cfg, conciergeStore(t), discardLogger, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-opening:
	case <-time.After(2 * time.Second):
		t.Fatal("control: the Envoy session's opening did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop(ctx)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the stop waited past its deadline for the Envoy session's opening")
	}
	close(release)
	waitUntil(t, "the session the opening completed after the stop closed", func() bool {
		late.mu.Lock()
		defer late.mu.Unlock()
		return late.closed
	})
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
