package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestACheckOfTheOldKeyDoesNotReplaceTheSavedKeysStatus fails if a check
// that began with the key before a save, and ends after a check of the saved
// key, caches its refusal over the saved key's result, as GPT-6.1 Sol's review
// of 743fe02 found: the settings page then reads Fails for five minutes for a
// key that works. The sequence is Sol's: a check held on the old key, the
// save (SetKeys), a fresh check with the saved key, then the old check's 401.
func TestACheckOfTheOldKeyDoesNotReplaceTheSavedKeysStatus(t *testing.T) {
	held, release := make(chan struct{}), make(chan struct{})
	var holdOnce sync.Once
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		if r.Header.Get("Authorization") != "Token k-2" {
			holdOnce.Do(func() { close(held) })
			<-release
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"projects":[]}`)
	}))
	defer srv.Close()
	keyed := func(key string) *config.Config {
		return &config.Config{
			Providers:    map[string]config.Provider{"deepgram": {API: config.APIDeepgram, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_DEEPGRAM_KEY"}}},
			ProviderKeys: map[string]string{"EXAMPLE_DEEPGRAM_KEY": key},
		}
	}
	set := New(keyed("k-1"), Options{})
	ctx := context.Background()

	old := make(chan []Status, 1)
	go func() { old <- set.Statuses(ctx, 0) }()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the check of the old key reached no provider")
	}
	set.SetKeys(keyed("k-2"))
	if fresh := set.Statuses(ctx, 0); len(fresh) != 1 || !fresh[0].OK {
		t.Fatalf("the check of the saved key: %+v; want it working", fresh)
	}
	close(release)
	if done := <-old; len(done) != 1 || done[0].OK {
		t.Fatalf("the control, the check of the old key: %+v; want its refusal", done)
	}

	before := requests.Load()
	cached := set.Statuses(ctx, time.Hour)
	if requests.Load() != before {
		t.Fatal("the read within the cache's age checked the provider again instead of reading the cache")
	}
	if len(cached) != 1 || !cached[0].OK {
		t.Fatalf("the cached status after the old key's check ended: %+v; want the saved key's result", cached)
	}
}
