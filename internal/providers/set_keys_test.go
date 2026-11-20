package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestSetKeysReachesAClientBuiltBefore fails if a key SetKeys gives a
// provider does not reach a client built from it before, or if the
// provider's last check survives the new key, so the settings page would
// show the old key's state until the check aged out.
func TestSetKeysReachesAClientBuiltBefore(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	cfg := func(key string) *config.Config {
		return &config.Config{
			Providers:    map[string]config.Provider{"dg": {API: config.APIDeepgram, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}}},
			ProviderKeys: map[string]string{"EXAMPLE_KEY": key},
		}
	}
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	set := New(cfg("k-1"), Options{Now: clk.Now})
	p, _ := set.Get("dg")
	client := p.HTTPClient(nil)
	set.Statuses(context.Background(), time.Hour) // a check with the first key

	set.SetKeys(cfg("k-2"))
	resp, err := client.Get(srv.URL + "/v1/projects")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	set.Statuses(context.Background(), time.Hour) // the new key's check, not the cached one

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != "Token k-1" || seen[1] != "Token k-2" || seen[2] != "Token k-2" {
		t.Fatalf("requests carried %q; want the first check with k-1, then the old client and a new check with k-2", seen)
	}
}
