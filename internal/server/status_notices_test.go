package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestAPIStatusSaysWhenEnvoyIsOffWithAHubNamed fails if GET /api/status gives
// no notice that a config naming an Envoy hub with Envoy off sends no session
// event and keeps each completed summary for later, and, with no
// concierge.envoy_url as well, that the voice cannot ask an issue's agent; if
// it says so in warnings, which the rollout's check and the acceptance scripts
// need empty while the office Pi keeps Envoy off for the hub's move
// (dispatch://INFRA-3862); or if a unit that names no hub, or has Envoy on by
// its file or its environment, gets a notice.
func TestAPIStatusSaysWhenEnvoyIsOffWithAHubNamed(t *testing.T) {
	const (
		hub      = "http://hub.example.test:9020"
		off      = "Envoy is off (envoy_enabled: false): no session event goes to the Envoy hub, and each completed summary waits until Envoy is on again."
		noAgents = " The voice cannot ask an issue's agent: no concierge.envoy_url."
	)
	for name, tc := range map[string]struct {
		yaml string
		env  map[string]string
		want []string
	}{
		"no hub named": {yaml: "envoy_enabled: false\n"},
		"on":           {yaml: "envoy_enabled: true\nenvoy_hub_url: " + hub + "\nconcierge:\n  envoy_url: " + hub + "\n"},
		"off, a hub named, no concierge.envoy_url": {
			yaml: "envoy_enabled: false\nenvoy_hub_url: " + hub + "\nconcierge:\n  envoy_url: \"\"\n",
			want: []string{off + noAgents},
		},
		"off, a hub named, a concierge.envoy_url": {
			yaml: "envoy_enabled: false\nenvoy_hub_url: " + hub + "\nconcierge:\n  envoy_url: " + hub + "\n",
			want: []string{off},
		},
		"off in the file, on by the environment": {
			yaml: "envoy_enabled: false\nenvoy_hub_url: " + hub + "\nconcierge:\n  envoy_url: \"\"\n",
			env:  map[string]string{config.EnvPrefix + "ENVOY_ENABLED": "true", config.EnvPrefix + "CONCIERGE_ENVOY_URL": hub},
		},
	} {
		t.Run(name, func(t *testing.T) {
			newTestConfigStore(t) // clears the environment
			t.Setenv(config.EnvPrefix+"CONCIERGE_ENVOY_URL", "")
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfgStore, _, err := config.NewStore(path)
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			rr := httptest.NewRecorder()
			newVoiceConfigHandler(t, cfgStore).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /api/status = %d %s", rr.Code, rr.Body.String())
			}
			var resp struct {
				Warnings []string  `json:"warnings"`
				Notices  *[]string `json:"notices"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode %s: %v", rr.Body.String(), err)
			}
			if resp.Notices == nil {
				t.Fatalf("GET /api/status has no notices list: %s", rr.Body.String())
			}
			if want := tc.want; !slices.Equal(*resp.Notices, want) && !(len(want) == 0 && len(*resp.Notices) == 0) {
				t.Errorf("notices = %q, want %q", *resp.Notices, want)
			}
			if len(resp.Warnings) != 0 {
				t.Errorf("warnings = %q, want none: an Envoy turned off on purpose is a notice", resp.Warnings)
			}
		})
	}
}
