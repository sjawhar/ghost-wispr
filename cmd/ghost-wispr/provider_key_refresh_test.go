package main

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
)

// TestASavedKeyReachesTheClientsBuiltBeforeIt fails if a provider key saved
// on the settings page reaches the providers check but not a client the unit
// built before the save: the search indexer and semantic search are built
// once at start, so a corrected key would read "Works" on the page while
// search kept sending the old one until a restart. A save of another role's
// model between the two (a new summarization.model builds a new
// providers.Set) must not cut the client off from the keys saved after it.
func TestASavedKeyReachesTheClientsBuiltBeforeIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(cfg *config.Config)
	}{
		{"keys only", func(*config.Config) {}},
		{"after another role's model was saved", func(cfg *config.Config) { cfg.Summarization.Model = "gw/chat-example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Header.Get("Authorization"))
				mu.Unlock()
				_, _ = io.WriteString(w, `{"object":"list","model":"emb-example","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}]}`)
			}))
			defer srv.Close()

			before := &config.Config{
				Providers: map[string]config.Provider{
					"gw": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_EMBED_KEY"}},
				},
				ProviderKeys: map[string]string{"EXAMPLE_EMBED_KEY": "k-1"},
				Embedding:    config.Embedding{Model: "gw/emb-example"},
			}
			client, _, err := newEmbeddingClient(before)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, err := client.Embed(ctx, []string{"one"}, embedding.TaskTypeDocument); err != nil {
				t.Fatal(err)
			}

			// The settings page saves another setting, then a new key: the
			// store calls the config-change callbacks, which take the
			// providers of the config saved last.
			changed := *before
			tc.before(&changed)
			unitProviders.of(&changed)
			after := changed
			after.ProviderKeys = maps.Clone(before.ProviderKeys)
			after.ProviderKeys["EXAMPLE_EMBED_KEY"] = "k-2"
			unitProviders.of(&after)

			if _, err := client.Embed(ctx, []string{"two"}, embedding.TaskTypeDocument); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if want := []string{"Bearer k-1", "Bearer k-2"}; !slices.Equal(seen, want) {
				t.Fatalf("the embedding client built before the save sent %q, want %q", seen, want)
			}
		})
	}
}
