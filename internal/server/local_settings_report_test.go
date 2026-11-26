package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestGetConfigReportsTheLocalSettingsTheStoreApplied fails if GET
// /api/config has no local_settings field, if its report of a local settings
// file in YAML's flow style does not hold that file's values or holds a
// provider's sign-in, if a file the store did not apply is reported otherwise
// than {"applied": false, "error": ...}, or no file otherwise than null. The
// acceptance driver, deploy/pi/check-deploy.sh, reads this field instead of the
// file (GPT-6.1 Sol's review of PR #201, finding 1).
func TestGetConfigReportsTheLocalSettingsTheStoreApplied(t *testing.T) {
	const release = "silence_timeout: 30s\n" +
		"providers:\n  vertex:\n    api: vertex\n    project: example-project\n    auth:\n      kind: google\n" +
		"summarization:\n  model: vertex/gemini-release\n"
	for name, tc := range map[string]struct {
		local string
		check func(t *testing.T, models map[string]string, report json.RawMessage)
	}{
		"flow style": {
			local: "summarization: {model: vertex/gemini-saved}\n" +
				"providers: {vertex-eu: {api: vertex, project: example-project, auth: {kind: google_id_token, audience: example-audience}}}\n",
			check: func(t *testing.T, models map[string]string, report json.RawMessage) {
				if models["summarization"] != "vertex/gemini-saved" {
					t.Errorf("control: the running summarization model is %q, want the local file's", models["summarization"])
				}
				var got any
				if err := json.Unmarshal(report, &got); err != nil {
					t.Fatal(err)
				}
				canonical, _ := json.Marshal(got)
				want := `{"providers":{"vertex-eu":{"api":"vertex","project":"example-project"}},"summarization":{"model":"vertex/gemini-saved"}}`
				if string(canonical) != want {
					t.Errorf("local_settings = %s, want %s", canonical, want)
				}
			},
		},
		"unknown keys": {
			local: "silence_timeout: 45s\nprovider_keys: {deepgram: dg-typed-by-hand}\ndispatch_token: token-typed-by-hand\napplied: false\n",
			check: func(t *testing.T, _ map[string]string, report json.RawMessage) {
				if string(report) != `{"silence_timeout":"45s"}` {
					t.Errorf(`local_settings = %s, want {"silence_timeout":"45s"}: a key the config does not read is served`, report)
				}
			},
		},
		"not applied": {
			local: "silence_timeout: [\n",
			check: func(t *testing.T, _ map[string]string, report json.RawMessage) {
				var got map[string]any
				if err := json.Unmarshal(report, &got); err != nil {
					t.Fatalf("local_settings %s is not an object: %v", report, err)
				}
				errText, _ := got["error"].(string)
				if got["applied"] != false || !strings.Contains(errText, "yaml:") || len(got) != 2 {
					t.Errorf(`local_settings = %s, want {"applied": false, "error": <the parse error>}`, report)
				}
			},
		},
		"none": {
			check: func(t *testing.T, _ map[string]string, report json.RawMessage) {
				if string(report) != "null" {
					t.Errorf("local_settings with no local settings file = %s, want null", report)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			newTestConfigStore(t) // clears the environment
			path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
			if err := os.WriteFile(path, []byte(release), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.local != "" {
				if err := os.WriteFile(config.LocalSettingsPath(path), []byte(tc.local), 0o644); err != nil {
					t.Fatal(err)
				}
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
			var resp map[string]json.RawMessage
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			report, ok := resp["local_settings"]
			if !ok {
				t.Fatalf("GET /api/config has no local_settings field: %s", rr.Body.String())
			}
			var models map[string]string
			if err := json.Unmarshal(resp["models"], &models); err != nil {
				t.Fatal(err)
			}
			tc.check(t, models, report)
		})
	}
}

// TestOpenAPISpecDescribesTheLocalSettingsReport fails if the OpenAPI spec of
// GET /api/config does not describe local_settings: the settings applied from
// ghost-wispr.local.yaml, with no provider's auth, the form of a file not
// applied, and null for none.
func TestOpenAPISpecDescribesTheLocalSettingsReport(t *testing.T) {
	data, err := json.Marshal(configSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	property, _ := schema["properties"].(map[string]any)["local_settings"].(map[string]any)
	if property == nil {
		t.Fatal("the OpenAPI config schema has no local_settings")
	}
	description, _ := property["description"].(string)
	for _, words := range []string{"ghost-wispr.local.yaml", "auth", `"applied": false`, "null", "GET only"} {
		if !strings.Contains(description, words) {
			t.Errorf("local_settings description %q does not say %q", description, words)
		}
	}
}
