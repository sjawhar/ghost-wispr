package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// searchIndexWarning is the index warning refusingGateway's refusal sets.
const searchIndexWarning = "Search cannot index sessions with gateway/gemini-embedding-2: gateway answered HTTP 403 (team_model_access_denied); the provider refused this model for this sign-in"

// indexed says whether session id has embeddings stored.
func indexed(t *testing.T, store *storage.SQLiteStore, id string) bool {
	t.Helper()
	stored, err := store.GetEmbeddings(id)
	if err != nil {
		t.Fatal(err)
	}
	return len(stored) > 0
}

// TestAGoodQuestionLeavesTheIndexWarning fails if a question semantic search
// answers clears the warning of an index the provider refused: the sessions
// that index missed are still missing from search, and nothing would say so.
// It also fails if a refused question does not warn, or a good one does not
// clear that warning.
func TestAGoodQuestionLeavesTheIndexWarning(t *testing.T) {
	var refuse atomic.Bool
	refuse.Store(true)
	client, model, ref := refusingGateway(t, &refuse)
	store, _, _ := newDeletionTestStore(t, "recording.mp3")
	if _, _, err := store.Canonicalize("session"); err != nil {
		t.Fatal(err)
	}
	warnings := newWarningsRegistry()
	if err := newSearchIndexer(t.Context(), client, model, ref, store, warnings).BackfillMissing(t.Context()); err == nil {
		t.Fatal("a backfill the provider refused succeeded")
	}
	queries := searchQueryClient(client, ref, warnings)
	const questionWarning = "Search cannot answer a question with gateway/gemini-embedding-2: gateway answered HTTP 403 (team_model_access_denied); the provider refused this model for this sign-in"

	if _, err := queries.Embed(t.Context(), []string{"bridge"}, embedding.TaskTypeQuery); err == nil {
		t.Fatal("a question the provider refused was answered")
	}
	if got, want := warnings.List(), []string{searchIndexWarning, questionWarning}; !slices.Equal(got, want) {
		t.Fatalf("after a refused index and a refused question, status warnings = %q, want %q", got, want)
	}

	refuse.Store(false)
	if _, err := queries.Embed(t.Context(), []string{"bridge"}, embedding.TaskTypeQuery); err != nil {
		t.Fatalf("a question the provider took: %v", err)
	}
	if got, want := warnings.List(), []string{searchIndexWarning}; !slices.Equal(got, want) {
		t.Fatalf("after a good question, status warnings = %q, want only the index's %q: the session is still not indexed", got, want)
	}
	if indexed(t, store, "session") {
		t.Fatal("the control: a question indexed the session")
	}
}

// TestAGoodIndexAfterARefusalIndexesTheSessionsMissed fails if, once the
// provider takes an index again after refusing the start's backfill, the
// sessions that backfill missed stay out of search until a restart, or the
// index warning clears before they are indexed.
func TestAGoodIndexAfterARefusalIndexesTheSessionsMissed(t *testing.T) {
	var refuse atomic.Bool
	refuse.Store(true)
	client, model, ref := refusingGateway(t, &refuse)
	store, _, started := newDeletionTestStore(t, "recording.mp3")
	if _, _, err := store.Canonicalize("session"); err != nil {
		t.Fatal(err)
	}
	const later = "later"
	if err := store.CreateSession(later, started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment(later, transcribe.Segment{Text: "The later meeting moved the launch.", EndTime: 2, Timestamp: started.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(later, started.Add(time.Hour+time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	transcript, generation, err := store.Canonicalize(later)
	if err != nil {
		t.Fatal(err)
	}
	warnings := newWarningsRegistry()
	indexer := newSearchIndexer(t.Context(), client, model, ref, store, warnings)

	indexer.requestBackfill() // the start's
	waitUntil(t, "the start's refused backfill to warn", func() bool { return slices.Equal(warnings.List(), []string{searchIndexWarning}) })
	if indexed(t, store, "session") || indexed(t, store, later) {
		t.Fatal("a backfill the provider refused indexed a session")
	}

	refuse.Store(false)
	if err := indexer.IndexSessionAtGeneration(t.Context(), later, transcript, generation); err != nil {
		t.Fatalf("an index the provider took: %v", err)
	}
	waitUntil(t, "the index warning to clear", func() bool { return len(warnings.List()) == 0 })
	if !indexed(t, store, "session") {
		t.Fatal("the index warning cleared, but the session the start's backfill missed is still not indexed")
	}
}

// keyedEmbeddings is an OpenAI-API embeddings endpoint that answers 401
// invalid_api_key to any key but k-2. Each request with k-2 waits for
// release; reached is closed at the first.
type keyedEmbeddings struct {
	url     string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newKeyedEmbeddings(t *testing.T) *keyedEmbeddings {
	t.Helper()
	k := &keyedEmbeddings{reached: make(chan struct{}), release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer k-2" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		k.once.Do(func() { close(k.reached) })
		select {
		case <-k.release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "model": "emb-example",
			"data": []map[string]any{{"object": "embedding", "index": 0, "embedding": []float32{0.1, 0.2}}},
		})
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-k.release:
		default:
			close(k.release)
		}
	})
	k.url = srv.URL
	return k
}

// TestASavedKeyIndexesTheSessionsTheStartMissed fails if a key saved on the
// settings page (PATCH /api/config provider_keys, as main wires the config
// store and the search indexer) for the embedding provider that refused the
// start's backfill leaves the sessions that backfill missed out of search
// until a restart; or if the index warning clears before they are indexed.
func TestASavedKeyIndexesTheSessionsTheStartMissed(t *testing.T) {
	const keyEnv = "EXAMPLE_EMBED_KEY"
	fake := newKeyedEmbeddings(t)
	t.Setenv(keyEnv, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "ghost-wispr.yaml")
	if err := os.WriteFile(path, fmt.Appendf(nil, "providers:\n  fakeemb: {api: openai, url: %s/v1, auth: {kind: api_key, env: %s}}\nembedding: {model: fakeemb/emb-example}\n", fake.url, keyEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(keyEnv+"=k-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgStore, _, err := loadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	store, _, _ := newDeletionTestStore(t, "recording.mp3")
	if _, _, err := store.Canonicalize("session"); err != nil {
		t.Fatal(err)
	}
	cfg := cfgStore.Get()
	client, model, err := newEmbeddingClient(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	warnings := newWarningsRegistry()
	indexer := newSearchIndexer(t.Context(), client, model, cfg.Embedding.Model, store, warnings)
	indexer.followSignIn(cfgStore)
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, &server.ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Search cannot index sessions with fakeemb/emb-example: fakeemb answered HTTP 401 (invalid_api_key)"

	indexer.requestBackfill() // the start's
	waitUntil(t, "the start's backfill, refused for k-1, to warn", func() bool {
		return strings.HasPrefix(strings.Join(warnings.List(), "\n"), want)
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"provider_keys":{"fakeemb":"k-2"}}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH provider_keys = %d %s", rr.Code, rr.Body.String())
	}
	select {
	case <-fake.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("after the key was saved, no index reached the provider with it: the missed session waits for a restart")
	}
	if got := strings.Join(warnings.List(), "\n"); !strings.HasPrefix(got, want) || indexed(t, store, "session") {
		t.Fatalf("while the backfill with the saved key runs: status warnings %q, session indexed %t; want the index warning kept and the session not yet indexed", got, indexed(t, store, "session"))
	}
	close(fake.release)
	waitUntil(t, "the index warning to clear", func() bool { return len(warnings.List()) == 0 })
	if !indexed(t, store, "session") {
		t.Fatal("the index warning cleared, but the session is still not indexed")
	}
}

// patchConfig sends PATCH /api/config with body to h and fails the test
// unless it answers 200.
func patchConfig(t *testing.T, h http.Handler, body string) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /api/config %s = %d %s", body, rr.Code, rr.Body.String())
	}
}

// TestARoleSaveThenAKeySaveReachesTheSearchClients fails if a corrected key,
// saved on the settings page after a save of summarization.model (a second
// model on the same provider, which builds a new providers.Set), leaves the
// search indexer and semantic search, built at start, on the old key, as
// GPT-6.1 Sol's review of e9c6fb8 found: the providers check would read the
// new key while every index and question was refused until a restart. Both
// saves go through PATCH /api/config, with main's config-change callbacks.
func TestARoleSaveThenAKeySaveReachesTheSearchClients(t *testing.T) {
	const keyEnv = "EXAMPLE_AI_KEY"
	var valid atomic.Value
	valid.Store("Bearer k-1")
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		auth := r.Header.Get("Authorization")
		mu.Lock()
		sent = append(sent, auth)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if auth != valid.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","model":"emb-example","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}]}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(keyEnv, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "ghost-wispr.yaml")
	if err := os.WriteFile(path, fmt.Appendf(nil, "providers:\n  fakeai: {api: openai, url: %s/v1, auth: {kind: api_key, env: %s}}\nsummarization: {model: fakeai/chat-a}\nembedding: {model: fakeai/emb-example}\n", srv.URL, keyEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(keyEnv+"=k-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgStore, _, err := loadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	store, _, _ := newDeletionTestStore(t, "recording.mp3")
	transcript, generation, err := store.Canonicalize("session")
	if err != nil {
		t.Fatal(err)
	}
	// The start, as main builds search.
	cfg := cfgStore.Get()
	client, model, err := newEmbeddingClient(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	warnings := newWarningsRegistry()
	indexer := newSearchIndexer(t.Context(), client, model, cfg.Embedding.Model, store, warnings)
	queries := searchQueryClient(client, cfg.Embedding.Model, warnings)
	// main's first config-change callback takes the providers of the config
	// saved last; the indexer follows embedding.model's sign-in.
	onConfigChange(cfgStore, func(cfg *config.Config) { unitProviders.of(cfg) })
	indexer.followSignIn(cfgStore)
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, &server.ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}

	// The control: the key the unit started with works.
	if _, err := queries.Embed(t.Context(), []string{"bridge"}, embedding.TaskTypeQuery); err != nil {
		t.Fatalf("a question with the key the unit started with: %v", err)
	}
	patchConfig(t, h, `{"summarization":{"model":"fakeai/chat-b"}}`)
	// The provider now takes only the corrected key, which the page saves.
	valid.Store("Bearer k-2")
	patchConfig(t, h, `{"provider_keys":{"fakeai":"k-2"}}`)
	mu.Lock()
	saved := len(sent)
	mu.Unlock()

	if _, err := queries.Embed(t.Context(), []string{"bridge"}, embedding.TaskTypeQuery); err != nil {
		t.Errorf("a question after the role and key saves: %v", err)
	}
	if err := indexer.IndexSessionAtGeneration(t.Context(), "session", transcript, generation); err != nil {
		t.Errorf("an index after the role and key saves: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, auth := range sent[saved:] {
		if auth != "Bearer k-2" {
			t.Fatalf("after the key was saved, search sent %q (all after the save: %q), want only the saved key", auth, sent[saved:])
		}
	}
}
