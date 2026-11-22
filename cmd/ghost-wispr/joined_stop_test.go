package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice/joined"
)

// noticeHoldingAgents is the unit's Envoy session: lookups answer live, a
// join's answer goes at once, and each voice-stopped notice waits until its
// context ends, as an HTTP send to a slow Envoy does.
type noticeHoldingAgents struct {
	recordingAgents
	mu      sync.Mutex
	notices map[string]bool
}

func (a *noticeHoldingAgents) Send(ctx context.Context, r *envoy.SendRequest) (envoy.SendResult, error) {
	if !strings.Contains(r.Message, "meeting voice stopped") {
		return a.recordingAgents.Send(ctx, r)
	}
	a.mu.Lock()
	a.notices[r.Target] = true
	a.mu.Unlock()
	<-ctx.Done()
	return envoy.SendResult{}, ctx.Err()
}

// TestAStopTellsTheJoinedAgentsOnlyWithinItsDeadline fails if the
// Concierge's stop, as main runs it at shutdown (startConcierge's stop), its
// deadline 100 ms off, waits past that deadline to tell three agents still
// joined that the voice stopped: the notices run under the stop's context,
// all at once. Before, they went one at a time under the roster's own
// context, ten seconds each. The control has no agent joined. From review
// 6006723807 of #138.
func TestAStopTellsTheJoinedAgentsOnlyWithinItsDeadline(t *testing.T) {
	members := []string{"agent-1", "agent-2", "agent-3"}
	for _, joinedCount := range []int{0, len(members)} {
		name := "control: no agent joined"
		if joinedCount > 0 {
			name = fmt.Sprintf("%d agents joined", joinedCount)
		}
		t.Run(name, func(t *testing.T) {
			agents := &noticeHoldingAgents{notices: map[string]bool{}}
			session := &fakeEnvoySession{Agents: agents}
			delivers := make(chan func(context.Context, *envoy.Message) error, 1)
			original := openEnvoySession
			t.Cleanup(func() { openEnvoySession = original })
			openEnvoySession = func(_ context.Context, _ *envoy.Listener, cfg *envoy.SessionConfig) (envoySession, error) {
				delivers <- cfg.Deliver
				return session, nil
			}
			var link voiceLink
			roster := testRoster(t, &link)
			cfg := geminiKeyConfig("test")
			cfg.Concierge.EnvoyURL = "http://127.0.0.1:1"
			_, stop, err := startConcierge(t.Context(), &cfg, conciergeStore(t), discardLogger, nil, roster)
			if err != nil {
				t.Fatal(err)
			}
			var deliver func(context.Context, *envoy.Message) error
			select {
			case deliver = <-delivers:
			case <-time.After(2 * time.Second):
				t.Fatal("control: the Envoy session did not open")
			}
			for i, agent := range members[:joinedCount] {
				join := &envoy.Message{EventID: fmt.Sprintf("e%d", i), SourceSession: agent, Text: "ghost-wispr join"}
				deadline := time.Now().Add(2 * time.Second)
				for {
					// Until the roster takes the session, it refuses the join
					// with ErrNotReady, and Envoy delivers it again.
					err := deliver(t.Context(), join)
					if err != nil && !errors.Is(err, joined.ErrNotReady) {
						t.Fatalf("deliver %s's join: %v", agent, err)
					}
					if err == nil || time.Now().After(deadline) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if got := roster.Joined(); len(got) != joinedCount {
				t.Fatalf("control: %d agents joined, want %d", len(got), joinedCount)
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
				t.Fatal("the stop waited past its deadline to tell the joined agents the voice stopped")
			}
			if got := roster.Joined(); len(got) != 0 {
				t.Errorf("joined after the stop: %+v, want none", got)
			}
			agents.mu.Lock()
			defer agents.mu.Unlock()
			if len(agents.notices) != joinedCount {
				t.Errorf("voice-stopped notices begun for %d agents, want all %d", len(agents.notices), joinedCount)
			}
		})
	}
}
