package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// countingHub answers each Envoy publish as delivered, with no network.
type countingHub struct{ sends atomic.Int32 }

func (h *countingHub) RoundTrip(req *http.Request) (*http.Response, error) {
	h.sends.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"event_id":"local"}`)), Request: req}, nil
}

// TestARecoveredEndKeepsItsEventThroughAStopBeforeItsSend fails if the
// start's recovery of a session a power cut left active (recoverStaleSessions)
// saves its end without its session_ended event, when Envoy events are on:
// a second stop before publishRecoveredEnds sends it would lose it, since the
// session is no longer active at the next start and nothing recovers it
// again. The next start's sweep must find the event and send it once. The
// control has Envoy events off, and queues nothing. From review 6006372566
// of #138.
func TestARecoveredEndKeepsItsEventThroughAStopBeforeItsSend(t *testing.T) {
	for _, on := range []bool{false, true} {
		name := "control: Envoy events are off"
		if on {
			name = "Envoy events are on"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			store := openResumeStore(t, path)
			const id = "20261121080000"
			if err := store.CreateSession(id, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{EnvoyEnabled: on, EnvoyHubURL: "http://hub.invalid", AudioDir: t.TempDir()}
			if ids := recoverStaleSessions(store, &cfg); len(ids) != 1 || ids[0] != id {
				t.Fatalf("control: recovered %v, want [%s]", ids, id)
			}
			// The second stop comes here, before publishRecoveredEnds.
			if !closeStore(context.Background(), store) {
				t.Fatal("closeStore left the store open")
			}
			next := openResumeStore(t, path)
			if ids := recoverStaleSessions(next, &cfg); len(ids) != 0 {
				t.Fatalf("control: the next start recovered %v again, want none", ids)
			}
			pending, err := next.PendingEnvoyLifecycleEvents()
			if err != nil {
				t.Fatal(err)
			}
			if !on {
				if len(pending) != 0 {
					t.Fatalf("control: %d lifecycle events queued with Envoy events off, want none", len(pending))
				}
				return
			}
			if len(pending) != 1 || pending[0].SessionID != id || pending[0].Type != envoy.SessionEndedEvent {
				t.Fatalf("queued after the stop: %+v, want the recovered session's session_ended", pending)
			}
			hub := &countingHub{}
			publisher, err := envoy.NewPublisher(cfg.EnvoyHubURL, next, envoy.WithHTTPClient(&http.Client{Transport: hub}))
			if err != nil {
				t.Fatal(err)
			}
			if err := publisher.PublishPendingLifecycleEvents(context.Background()); err != nil { // the next start's sweep
				t.Fatal(err)
			}
			if pending, err = next.PendingEnvoyLifecycleEvents(); err != nil || len(pending) != 0 || hub.sends.Load() != 1 {
				t.Fatalf("after the sweep: %d sends, queued %+v (%v), want the event sent once", hub.sends.Load(), pending, err)
			}
		})
	}
}
