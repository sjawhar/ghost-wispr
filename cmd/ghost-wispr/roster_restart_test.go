package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// TestARestartedVoiceKeepsItsRoster fails if the voice turned off while its
// Envoy session still opens, and on again, loses the roster of the new
// start to the old one: the old opening, completing once the new session is
// open and an agent joined under it, must not install its session, and the
// old start's stop, closing the old session, must not end the new start's
// membership or close the new session. Each start opens the roster on its
// own (joined.Opening). The control turns the voice off and on in order:
// the old opening completes, and the old stop ends, before the new start.
// From review 6006965341 of #138 (Sol's TestReview138RapidVoiceRestart).
func TestARestartedVoiceKeepsItsRoster(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		name := "control: the old start stops before the new one"
		if overlap {
			name = "the old opening completes after the new start joined an agent"
		}
		t.Run(name, func(t *testing.T) {
			firstOpening, oldCancelled, releaseOld := make(chan struct{}), make(chan struct{}), make(chan struct{})
			secondDeliver := make(chan func(context.Context, *envoy.Message) error, 1)
			first := &fakeEnvoySession{Agents: &recordingAgents{}}
			second := &fakeEnvoySession{Agents: &recordingAgents{}}
			original := openEnvoySession
			t.Cleanup(func() { openEnvoySession = original })
			calls := 0
			openEnvoySession = func(ctx context.Context, _ *envoy.Listener, cfg *envoy.SessionConfig) (envoySession, error) {
				calls++
				if calls == 1 {
					close(firstOpening)
					<-ctx.Done() // the voice turned off
					close(oldCancelled)
					<-releaseOld // and the opening completes anyway
					return first, nil
				}
				secondDeliver <- cfg.Deliver
				return second, nil
			}
			var link voiceLink
			roster := testRoster(t, &link)
			cfg := config.Config{GenAIBackend: "gemini", GeminiAPIKey: "test", Concierge: config.Concierge{EnvoyURL: "http://127.0.0.1:1"}}
			lazy := newLazyConcierge(func() (*concierge.Concierge, func(context.Context), error) {
				return startConcierge(t.Context(), &cfg, conciergeStore(t), discardLogger, nil, roster)
			}, nil, discardLogger)
			defer lazy.Stop(context.Background())
			if _, err := lazy.start(); err != nil {
				t.Fatal(err)
			}
			<-firstOpening
			off := make(chan struct{})
			go func() {
				defer close(off)
				lazy.followEnabled(false)
			}()
			<-oldCancelled
			if !overlap {
				close(releaseOld)
				<-off
			}
			lazy.followEnabled(true)
			var deliver func(context.Context, *envoy.Message) error
			select {
			case deliver = <-secondDeliver:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the new start's Envoy session did not open")
			}
			if err := deliver(t.Context(), &envoy.Message{EventID: "new-join", SourceSession: "agent-new", Text: "ghost-wispr join"}); err != nil {
				t.Fatal(err)
			}
			if len(roster.Joined()) != 1 {
				t.Fatal("control: the new start's roster did not take the join")
			}
			if overlap {
				close(releaseOld)
				select {
				case <-off:
				case <-time.After(5 * time.Second):
					t.Fatal("the old start's stop did not end")
				}
			}
			first.mu.Lock()
			oldClosed := first.closed
			first.mu.Unlock()
			second.mu.Lock()
			newClosed := second.closed
			second.mu.Unlock()
			if got := roster.Joined(); !oldClosed || newClosed || len(got) != 1 || got[0].Agent != "agent-new" {
				t.Errorf("old session closed: %t, new session closed: %t, roster %+v; want true, false and agent-new", oldClosed, newClosed, got)
			}
			if _, err := roster.Invitation("ABC-12"); err != nil {
				t.Errorf("the restarted voice's roster has no Envoy session: %v", err)
			}
		})
	}
}
