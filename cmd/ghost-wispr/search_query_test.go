package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// searchHandler is the unit's web API with semantic search on queries, as
// main wires it.
func searchHandler(t *testing.T, store *storage.SQLiteStore, queries embedding.Client) http.Handler {
	t.Helper()
	h, err := server.HandlerWithLogger(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, &server.ControlHooks{}, "", nil, slog.Default(), queries)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// askSemantic sends GET /api/search/semantic with a question.
func askSemantic(h http.Handler) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/search/semantic?q=bridge", nil))
	return rr
}

// TestARefusedSearchQueryWarnsOnTheStatusPage fails if GET
// /api/search/semantic, whose question the provider refuses (a gateway's 403
// team_model_access_denied), logs nothing or leaves /api/status without the
// search warning naming embedding.model and the provider's status and code;
// if the warning or the log quotes the provider's message; if the next
// question that works leaves the warning, or loses the model the answer
// names; or if search with embeddings off stops answering 501.
func TestARefusedSearchQueryWarnsOnTheStatusPage(t *testing.T) {
	keepDefaultLogger(t)
	var logged cliOutput
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	var refuse atomic.Bool
	client, model, ref := refusingGateway(t, &refuse)
	store, _, _ := newDeletionTestStore(t, "recording.mp3")
	transcript, generation, err := store.Canonicalize("session")
	if err != nil {
		t.Fatal(err)
	}
	warnings := newWarningsRegistry()
	if err := newSearchIndexer(t.Context(), nil, client, model, ref, store, warnings).IndexSessionAtGeneration(t.Context(), "session", transcript, generation); err != nil {
		t.Fatalf("index the session to search: %v", err)
	}
	h := searchHandler(t, store, searchQueryClient(client, ref, warnings))

	refuse.Store(true)
	if rr := askSemantic(h); rr.Code != http.StatusInternalServerError {
		t.Fatalf("a question the provider refused answered %d %s, want 500", rr.Code, rr.Body.String())
	}
	const want = "Search cannot answer a question with gateway/gemini-embedding-2: gateway answered HTTP 403 (team_model_access_denied); the provider refused this model for this sign-in"
	if got := strings.Join(warnings.List(), "\n"); got != want {
		t.Fatalf("after a refused question, status warnings = %q, want %q", got, want)
	}
	if got := logged.String(); !strings.Contains(got, "semantic search") || !strings.Contains(got, "team_model_access_denied") || strings.Contains(got, plantedRefusal) {
		t.Fatalf("logged %q, want a semantic search line with the provider's code and none of its message", got)
	}

	refuse.Store(false)
	rr := askSemantic(h)
	var answer struct {
		Model string `json:"model"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &answer) != nil || answer.Model != "gemini-embedding-2" {
		t.Fatalf("a question the provider took answered %d %s, want 200 naming the model the answer names", rr.Code, rr.Body.String())
	}
	if got := warnings.List(); len(got) != 0 {
		t.Fatalf("after a question that worked, status warnings = %q, want none", got)
	}

	if rr := askSemantic(searchHandler(t, store, searchQueryClient(nil, ref, warnings))); rr.Code != http.StatusNotImplemented {
		t.Fatalf("search with embeddings off answered %d, want 501", rr.Code)
	}
}
