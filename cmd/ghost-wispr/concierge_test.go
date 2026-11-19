package main

import (
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
			c, stop, err := startConcierge(t.Context(), &cfg, store, discardLogger)
			if err != nil {
				t.Fatalf("startConcierge: %v", err)
			}
			defer stop()
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
	if c, stop, err := startConcierge(t.Context(), &cfg, conciergeStore(t), discardLogger); err == nil {
		stop()
		t.Fatalf("started a Concierge %v with no model credentials", c)
	}
}
