package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

// pausedSnapshot is a config store as a reader sees it: its first Get takes
// the store's config, then waits for release before handing it over, so the
// reader goes on with a snapshot older than a save made meanwhile.
type pausedSnapshot struct {
	store   interface{ Get() config.Config }
	took    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *pausedSnapshot) Get() config.Config {
	cfg := p.store.Get()
	p.once.Do(func() {
		close(p.took)
		<-p.release
	})
	return cfg
}

// TestAStaleReaderDoesNotRestoreAReplacedKey fails if a reader of the
// providers (the settings page's check, providerStatuses) that took its
// config before a key save, and reaches the unit's providers after the save
// was applied, gives the clients the unit built before the save the key the
// save replaced, as GPT-6.1 Sol's review of 4e85fcf found: search would then
// be refused with the rejected key until the next save or a restart. The
// save goes through PATCH /api/config with main's config-change callback.
func TestAStaleReaderDoesNotRestoreAReplacedKey(t *testing.T) {
	const keyEnv = "EXAMPLE_EMBED_KEY"
	var valid atomic.Value
	valid.Store("Bearer k-1")
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		auth := r.Header.Get("Authorization")
		if r.URL.Path == "/v1/embeddings" {
			mu.Lock()
			sent = append(sent, auth)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if auth != valid.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"emb-example"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","model":"emb-example","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}]}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(keyEnv, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "ghost-wispr.yaml")
	if err := os.WriteFile(path, fmt.Appendf(nil, "providers:\n  fakeai: {api: openai, url: %s/v1, auth: {kind: api_key, env: %s}}\nembedding: {model: fakeai/emb-example}\n", srv.URL, keyEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(keyEnv+"=k-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgStore, _, err := loadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// The start, as main wires it: the unit's providers follow the store,
	// search is built once, and the first config-change callback takes the
	// providers of the config saved last.
	unitProviders.follow(cfgStore)
	t.Cleanup(func() { unitProviders.follow(nil) })
	cfg := cfgStore.Get()
	client, _, err := newEmbeddingClient(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	queries := searchQueryClient(client, cfg.Embedding.Model, newWarningsRegistry())
	onConfigChange(cfgStore, func(cfg *config.Config) { unitProviders.of(cfg) })
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), nil, &server.ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}
	ask := func() error {
		_, err := queries.Embed(t.Context(), []string{"bridge"}, embedding.TaskTypeQuery)
		return err
	}
	if err := ask(); err != nil {
		t.Fatalf("the control, a question with the key the unit started with: %v", err)
	}

	// The settings page's check takes its config, and is held there.
	reader := &pausedSnapshot{store: cfgStore, took: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		providerStatuses(reader)(t.Context(), 0)
	}()
	select {
	case <-reader.took:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader took no config")
	}
	// The key is saved, and applied, while the reader holds the old config.
	valid.Store("Bearer k-2")
	patchConfig(t, h, `{"provider_keys":{"fakeai":"k-2"}}`)
	if err := ask(); err != nil {
		t.Fatalf("the control, a question after the key save: %v", err)
	}
	close(reader.release)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the reader did not finish")
	}

	mu.Lock()
	before := len(sent)
	mu.Unlock()
	if err := ask(); err != nil {
		t.Errorf("a question after the reader of the old config finished: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := sent[before:]; len(got) != 1 || got[0] != "Bearer k-2" {
		t.Errorf("after the reader of the old config finished, search sent %q; want the saved key, Bearer k-2", got)
	}
}
