package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func conciergeStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestTheConciergeStartsWithoutReach fails if the Concierge does not start,
// or start-up waits, when the unit has no Dispatch token, or cannot reach
// Envoy: the voice must still answer, saying it cannot reach Dispatch yet.
func TestTheConciergeStartsWithoutReach(t *testing.T) {
	cases := map[string]config.Config{
		"no token, no Envoy": {GenAIBackend: "gemini", GeminiAPIKey: "test"},
		"token, no Dispatch URL, Envoy down": {
			GenAIBackend: "gemini", GeminiAPIKey: "test", DispatchToken: "token",
			Concierge: config.Concierge{EnvoyURL: "http://127.0.0.1:1"},
		},
		"token and Dispatch URL": {
			GenAIBackend: "gemini", GeminiAPIKey: "test", DispatchToken: "token",
			Concierge: config.Concierge{DispatchURL: "http://127.0.0.1:1"},
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			store := conciergeStore(t)
			started := time.Now()
			c, stop, err := startConcierge(t.Context(), &cfg, store, discardLogger, nil, nil)
			if err != nil {
				t.Fatalf("startConcierge: %v", err)
			}
			defer stop(context.Background())
			if c == nil {
				t.Fatal("no Concierge")
			}
			if d := time.Since(started); d > time.Second {
				t.Fatalf("start-up waited %v for unreachable services", d)
			}
		})
	}
}

// TestTheConciergeNeedsAModel fails if a Concierge starts with no way to call
// its model.
func TestTheConciergeNeedsAModel(t *testing.T) {
	cfg := config.Config{GenAIBackend: "gemini"}
	if c, stop, err := startConcierge(t.Context(), &cfg, conciergeStore(t), discardLogger, nil, nil); err == nil {
		stop(context.Background())
		t.Fatalf("started a Concierge %v with no model credentials", c)
	}
}

// TestTheConciergeTellsTheSessionPageOfChanges fails if a change to a
// session's questions to agents (here, a meeting's end cancelling its open
// question) is not passed to the hook main connects to the voice_tasks_changed
// event, so an open session page would never show it.
func TestTheConciergeTellsTheSessionPageOfChanges(t *testing.T) {
	cfg := config.Config{GenAIBackend: "gemini", GeminiAPIKey: "test"}
	store := conciergeStore(t)
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("meeting", at); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateVoiceTask(&storage.VoiceTask{ID: "task", SessionID: "meeting", AskedAt: at, IssueKey: "ABC-1", Holder: "session:agent"}); err != nil {
		t.Fatal(err)
	}
	changed := make(chan string, 4)
	c, stop, err := startConcierge(t.Context(), &cfg, store, discardLogger, func(id string) { changed <- id }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	if err := c.SessionEnded("meeting"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-changed:
		if got != "meeting" {
			t.Fatalf("told of %q, want meeting", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the session page was not told of the change")
	}
}
