package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// plantedRefusal is a word only the refusing gateway's message holds: a
// warning or a log line that quotes the message would show it.
const plantedRefusal = "plantedrefusalword"

// refusingGateway is a model gateway's embeddings, as embedding.model
// gateway/gemini-embedding-2 names them, that answer 403
// team_model_access_denied while refuse holds and a vector otherwise. It
// returns the embedding client of the unit's search, its model and ref.
func refusingGateway(t *testing.T, refuse *atomic.Bool) (client embedding.Client, model, ref string) {
	t.Helper()
	const keyEnv = "EXAMPLE_GATEWAY_KEY"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if refuse.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"team may not use this model `+plantedRefusal+`","type":"invalid_request_error","code":"team_model_access_denied"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "model": "gemini-embedding-2",
			"data": []map[string]any{{"object": "embedding", "index": 0, "embedding": []float32{0.1, 0.2}}},
		})
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"gateway": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
		},
		ProviderKeys: map[string]string{keyEnv: "sk-1"},
		Embedding:    config.Embedding{Model: "gateway/gemini-embedding-2"},
	}
	client, model, err := newEmbeddingClient(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	return client, model, cfg.Embedding.Model
}

// TestARefusedSearchIndexWarnsOnTheStatusPage fails if an embedding index
// the provider refuses (a gateway's 403 team_model_access_denied), at the
// start's backfill or at a session's end, leaves /api/status without a
// search warning naming embedding.model, the provider's status and code; if
// the warning quotes the provider's message, or says to check a sign-in that
// worked; or if the next index that works leaves it once the backfill it
// asks for finds no session missing.
func TestARefusedSearchIndexWarnsOnTheStatusPage(t *testing.T) {
	runParallel(t)
	var refuse atomic.Bool
	refuse.Store(true)
	client, model, ref := refusingGateway(t, &refuse)
	store, _, _ := newDeletionTestStore(t, "recording.mp3")
	// The backfill finds an ended session by its stored canonical transcript.
	transcript, generation, err := store.Canonicalize("session")
	if err != nil {
		t.Fatal(err)
	}
	warnings := newWarningsRegistry()
	indexer := newSearchIndexer(t.Context(), nil, client, model, ref, store, warnings)
	// The refusal names the provider as the config names it.
	const want = "Search cannot index sessions with gateway/gemini-embedding-2: gateway answered HTTP 403 (team_model_access_denied); the provider refused this model for this sign-in"

	if err := indexer.BackfillMissing(t.Context()); err == nil {
		t.Fatal("a backfill the provider refused succeeded")
	}
	if got := strings.Join(warnings.List(), "\n"); got != want {
		t.Fatalf("after a refused backfill, status warnings = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(warnings.List(), "\n"), plantedRefusal) {
		t.Fatal("the search warning quotes the provider's message")
	}

	refuse.Store(false)
	if err := indexer.IndexSessionAtGeneration(context.Background(), "session", transcript, generation); err != nil {
		t.Fatalf("an index the provider took: %v", err)
	}
	waitUntil(t, "the backfill the index that worked asked for to clear the warning", func() bool { return len(warnings.List()) == 0 })

	refuse.Store(true)
	if err := indexer.IndexSessionAtGeneration(context.Background(), "session", transcript+" more words", generation); err == nil {
		t.Fatal("an index the provider refused succeeded")
	}
	if got := strings.Join(warnings.List(), "\n"); got != want {
		t.Fatalf("after a refused session index, status warnings = %q, want %q", got, want)
	}
}

// TestATTSRefusalOfTheModelDoesNotBlameTheSignIn fails if a text-to-speech
// provider's 403, an answer to a sign-in that worked, tells the operator to
// check the sign-in, or if a 401 no longer does.
func TestATTSRefusalOfTheModelDoesNotBlameTheSignIn(t *testing.T) {
	warnings := newWarningsRegistry()
	source := &speechSource{warnings: warnings}
	for _, c := range []struct {
		status int
		code   string
		want   string
	}{
		{http.StatusForbidden, "model_not_allowed", "Text-to-speech with speech/tts-a failed: openai answered HTTP 403 (model_not_allowed); the provider refused this model for this sign-in"},
		{http.StatusUnauthorized, "invalid_api_key", "Text-to-speech with speech/tts-a failed: openai answered HTTP 401 (invalid_api_key); check its provider's sign-in under Model providers"},
	} {
		source.warn("speech/tts-a", &providererr.Error{Provider: "openai", Status: c.status, Code: c.code})
		if got := strings.Join(warnings.List(), "\n"); got != c.want {
			t.Errorf("HTTP %d: status warnings = %q, want %q", c.status, got, c.want)
		}
	}
}
