package server

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// voiceYAML is a unit's ghost-wispr.yaml with every voice field set to a value
// other than its default.
const voiceYAML = `speaker_enabled: true
voice:
  enabled: true
  engine: dgagent
  wake_phrases: [Wispr, Hey Wispr]
  gptlive:
    model: openai/gpt-live-example
  dgagent:
    provider: deepgram
    think_model: google/gemini-example
`

// newVoiceConfigStore returns a config store read from voiceYAML, with no
// GHOST_WISPR_ variable set, and the path of its file.
func newVoiceConfigStore(t *testing.T) (*config.Store, string) {
	t.Helper()
	newTestConfigStore(t) // clears the environment
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte(voiceYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgStore, _, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return cfgStore, path
}

func newVoiceConfigHandler(t *testing.T, cfgStore *config.Store) http.Handler {
	t.Helper()
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return h
}

// patchConfig sends body as PATCH /api/config.
func patchConfig(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// responseVoice decodes the voice object of a GET or PATCH /api/config answer.
func responseVoice(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp struct {
		Voice map[string]any `json:"voice"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	return resp.Voice
}

// TestGetConfigServesTheVoiceSettings fails if GET /api/config does not give
// the settings page voice.enabled and voice.engine as the unit holds them, or
// gives it another voice field, such as voice B's model or a removed
// tts_engine.
func TestGetConfigServesTheVoiceSettings(t *testing.T) {
	cfgStore, _ := newVoiceConfigStore(t)
	h := newVoiceConfigHandler(t, cfgStore)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d %s", rr.Code, rr.Body.String())
	}
	want := map[string]any{"enabled": true, "engine": "dgagent"}
	if got := responseVoice(t, rr); !reflect.DeepEqual(got, want) {
		t.Errorf("voice = %v, want %v", got, want)
	}
}

// TestPatchConfigChangesTheVoiceAndKeepsItsOtherFields fails if PATCH
// /api/config does not change voice.enabled and voice.engine, changes a voice
// field the patch does not name, or loses another voice field from the config
// or its file.
func TestPatchConfigChangesTheVoiceAndKeepsItsOtherFields(t *testing.T) {
	cfgStore, path := newVoiceConfigStore(t)
	h := newVoiceConfigHandler(t, cfgStore)

	for _, step := range []struct {
		body string
		want map[string]any
	}{
		{`{"voice":{"engine":"gptlive"}}`, map[string]any{"enabled": true, "engine": "gptlive"}},
		{`{"voice":{"enabled":false}}`, map[string]any{"enabled": false, "engine": "gptlive"}},
		{`{"voice":{"enabled":true,"engine":"tts"}}`, map[string]any{"enabled": true, "engine": "tts"}},
	} {
		rr := patchConfig(h, step.body)
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH %s = %d %s, want 200", step.body, rr.Code, rr.Body.String())
		}
		if got := responseVoice(t, rr); !reflect.DeepEqual(got, step.want) {
			t.Errorf("after PATCH %s: voice = %v, want %v", step.body, got, step.want)
		}
	}

	want := config.Voice{
		Enabled:     true,
		Engine:      "tts",
		WakePhrases: []string{"Wispr", "Hey Wispr"},
		// The listener's threads are never in the file: the default, 1.
		PausedListenerThreads: 1,
		GPTLive:               config.GPTLive{Model: "openai/gpt-live-example"},
		DGAgent:               config.DGAgent{Provider: "deepgram", ThinkModel: "google/gemini-example"},
	}
	if got := cfgStore.Get().Voice; !reflect.DeepEqual(got, want) {
		t.Errorf("config voice = %+v, want %+v", got, want)
	}
	reloaded, _, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("reload %s: %v", path, err)
	}
	if got := reloaded.Get().Voice; !reflect.DeepEqual(got, want) {
		t.Errorf("voice saved in the config file = %+v, want %+v", got, want)
	}
}

// TestPatchConfigRefusesAnUnknownVoice fails if PATCH /api/config accepts a
// voice.engine other than tts, gptlive and dgagent, answers the refusal in
// another shape than the other settings' {"error": ...}, or changes anything
// (the config, its file, a listener) when it refuses.
func TestPatchConfigRefusesAnUnknownVoice(t *testing.T) {
	for _, tc := range []struct {
		body  string
		field string
	}{
		{`{"voice":{"engine":"robot"}}`, "voice.engine"},
		{`{"voice":{"engine":""}}`, "voice.engine"},
		{`{"voice":{"enabled":false,"engine":"robot"}}`, "voice.engine"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			cfgStore, path := newVoiceConfigStore(t)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var announced atomic.Int32
			cfgStore.OnChange(func(config.Config) { announced.Add(1) })
			h := newVoiceConfigHandler(t, cfgStore)

			rr := patchConfig(h, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("PATCH %s = %d %s, want 400", tc.body, rr.Code, rr.Body.String())
			}
			var refusal map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &refusal); err != nil || len(refusal) != 1 || !strings.Contains(refusal["error"], tc.field) {
				t.Errorf("refusal %s (%v), want {\"error\": ...} naming %s", rr.Body.String(), err, tc.field)
			}
			if v := cfgStore.Get().Voice; !v.Enabled || v.Engine != "dgagent" {
				t.Errorf("a refused PATCH changed the voice: enabled %v, engine %q; want true, dgagent", v.Enabled, v.Engine)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
				t.Errorf("a refused PATCH rewrote the config file (%v):\n%s", err, after)
			}
			assertNoLocalSettings(t, path)
			if n := announced.Load(); n != 0 {
				t.Errorf("a refused PATCH announced %d config changes", n)
			}
		})
	}
}

// TestOpenAPISpecDocumentsTheVoiceSettings fails if the OpenAPI spec of GET
// and PATCH /api/config does not give voice.enabled as a boolean and the
// values voice.engine accepts in the standard terms, or still documents a
// removed voice field such as tts_engine.
func TestOpenAPISpecDocumentsTheVoiceSettings(t *testing.T) {
	raw, err := OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	for _, schema := range [][]string{
		{"paths", "/api/config", "get", "responses", "200", "content", "application/json", "schema"},
		{"paths", "/api/config", "patch", "requestBody", "content", "application/json", "schema"},
	} {
		voice, _ := dig(t, spec, append(slices.Clone(schema), "properties", "voice", "properties")...).(map[string]any)
		if got := slices.Sorted(maps.Keys(voice)); !slices.Equal(got, []string{"enabled", "engine"}) {
			t.Errorf("%v voice properties %v, want exactly enabled and engine", schema[:3], got)
		}
		if typ := dig(t, voice, "enabled", "type"); typ != "boolean" {
			t.Errorf("%v voice.enabled type %v, want boolean", schema[:3], typ)
		}
		if got, want := dig(t, voice, "engine", "enum"), []any{"tts", "gptlive", "dgagent"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%v voice.engine enum %v, want %v", schema[:3], got, want)
		}
		description, _ := dig(t, voice, "engine", "description").(string)
		for _, words := range []string{"text-to-speech", "speech-to-speech: OpenAI GPT-Live", "voice agent: Deepgram"} {
			if !strings.Contains(description, words) {
				t.Errorf("%v voice.engine description %q does not say %q", schema[:3], description, words)
			}
		}
	}
}
