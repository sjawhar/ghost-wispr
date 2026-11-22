package providers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestStatusesMixCachedAndFreshChecks catches concurrent writes to the result
// map when a page keeps some providers' checks and refreshes the others.
func TestStatusesMixCachedAndFreshChecks(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cfg := &config.Config{Providers: map[string]config.Provider{}}
	for n := range 64 {
		cfg.Providers[fmt.Sprintf("p-%d", n)] = config.Provider{
			API: config.APIOpenAI, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"},
		}
	}
	set := New(cfg, Options{Now: func() time.Time { return now }})
	for range 20 {
		for n := range 64 {
			name := fmt.Sprintf("p-%d", n)
			if n%2 == 0 {
				set.status[name] = Status{Name: name, Checked: true, CheckedAt: now}
			} else {
				delete(set.status, name)
			}
		}
		statuses := set.Statuses(context.Background(), 5*time.Minute)
		if len(statuses) != 64 {
			t.Fatalf("got %d statuses, want all 64 cached and fresh checks", len(statuses))
		}
		for _, status := range statuses {
			if !status.Checked {
				t.Errorf("provider %s was neither cached nor checked", status.Name)
			}
		}
	}
}
