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
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestGetConfigServesTheRecordingRules fails if GET /api/config does not give
// the web UI the unit's recording_rules as written and in order, or gives a
// unit with none anything but an empty list: the page shows its Recording
// rules button only when the list has a rule.
func TestGetConfigServesTheRecordingRules(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		want []string
	}{
		"rules": {
			yaml: "recording_rules:\n  - Anyone can pause the recorder.\n  - \"Ask <i>first</i> & then quote.\"\n",
			want: []string{"Anyone can pause the recorder.", "Ask <i>first</i> & then quote."},
		},
		"none": {yaml: "silence_timeout: 30s\n", want: []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			newTestConfigStore(t) // clears the environment
			t.Setenv(config.EnvPrefix+"RECORDING_RULES", "")
			path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfgStore, _, err := config.NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
				sessionsByDate: map[string][]storage.Session{},
				sessions:       map[string]storage.Session{},
				segments:       map[string][]transcribe.Segment{},
			}, &ControlHooks{}, "", nil, cfgStore)
			if err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /api/config = %d %s", rr.Code, rr.Body.String())
			}
			var resp struct {
				RecordingRules *[]string `json:"recording_rules"`
			}
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatal(err)
			}
			if resp.RecordingRules == nil {
				t.Fatal("GET /api/config has no recording_rules list")
			}
			if !slices.Equal(*resp.RecordingRules, tc.want) {
				t.Fatalf("recording_rules = %q, want %q", *resp.RecordingRules, tc.want)
			}
		})
	}
}
