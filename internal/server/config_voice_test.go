package server

import (
	"bytes"
	"encoding/json"
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
gcp_project: example-project
voice:
  enabled: true
  engine: dgagent
  wake_phrases: [Wispr, Hey Wispr]
  tts_engine: aura
  tts_model: aura-2-example-en
  tts_voice: example
  tts_location: europe-west4
  gptlive:
    openai_identity_provider_id: idp-example
    openai_service_account_id: sa-example
`

// newVoiceConfigStore returns a config store read from voiceYAML, with no
// GHOST_WISPR_ variable set, and the path of its file.
func newVoiceConfigStore(t *testing.T) (*config.Store, string) {
	t.Helper()
	newTestConfigStore(t) // clears the environment
	for _, key := range []string{"VOICE_ENABLED", "VOICE_ENGINE", "VOICE_TTS_ENGINE", "VOICE_TTS_MODEL", "VOICE_TTS_VOICE", "VOICE_TTS_LOCATION"} {
		t.Setenv(config.EnvPrefix+key, "")
	}
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

func patchVoiceConfig(h http.Handler, body string) *httptest.ResponseRecorder {
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
// the settings page voice.enabled, voice.engine and voice.tts_engine as the
// unit holds them, or gives it another voice field, such as voice B's OpenAI
// identity ids.
func TestGetConfigServesTheVoiceSettings(t *testing.T) {
	cfgStore, _ := newVoiceConfigStore(t)
	h := newVoiceConfigHandler(t, cfgStore)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d %s", rr.Code, rr.Body.String())
	}
	want := map[string]any{"enabled": true, "engine": "dgagent", "tts_engine": "aura"}
	if got := responseVoice(t, rr); !reflect.DeepEqual(got, want) {
		t.Errorf("voice = %v, want %v", got, want)
	}
}

// TestPatchConfigChangesTheVoiceAndKeepsItsOtherFields fails if PATCH
// /api/config does not change voice.enabled, voice.engine and
// voice.tts_engine, changes a voice field the patch does not name, or loses
// another voice field from the config or its file.
func TestPatchConfigChangesTheVoiceAndKeepsItsOtherFields(t *testing.T) {
	cfgStore, path := newVoiceConfigStore(t)
	h := newVoiceConfigHandler(t, cfgStore)

	for _, step := range []struct {
		body string
		want map[string]any
	}{
		{`{"voice":{"engine":"gptlive"}}`, map[string]any{"enabled": true, "engine": "gptlive", "tts_engine": "aura"}},
		{`{"voice":{"enabled":false,"tts_engine":"gemini"}}`, map[string]any{"enabled": false, "engine": "gptlive", "tts_engine": "gemini"}},
		{`{"voice":{"enabled":true,"engine":"tts"}}`, map[string]any{"enabled": true, "engine": "tts", "tts_engine": "gemini"}},
	} {
		rr := patchVoiceConfig(h, step.body)
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
		TTSEngine:   "gemini",
		TTSModel:    "aura-2-example-en",
		TTSVoice:    "example",
		TTSLocation: "europe-west4",
		// The listener's threads are never in the file: the default, 1.
		PausedListenerThreads: 1,
		GPTLive: config.GPTLive{
			OpenAIIdentityProviderID: "idp-example",
			OpenAIServiceAccountID:   "sa-example",
		},
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
// voice.engine other than tts, gptlive and dgagent or a voice.tts_engine other
// than gemini and aura, answers the refusal in another shape than the other
// settings' {"error": ...}, or changes anything (the config, its file, a
// listener) when it refuses.
func TestPatchConfigRefusesAnUnknownVoice(t *testing.T) {
	for _, tc := range []struct {
		body  string
		field string
	}{
		{`{"voice":{"engine":"robot"}}`, "voice.engine"},
		{`{"voice":{"engine":""}}`, "voice.engine"},
		{`{"voice":{"enabled":false,"engine":"robot"}}`, "voice.engine"},
		{`{"voice":{"tts_engine":"robot"}}`, "voice.tts_engine"},
		{`{"voice":{"engine":"tts","tts_engine":""}}`, "voice.tts_engine"},
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

			rr := patchVoiceConfig(h, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("PATCH %s = %d %s, want 400", tc.body, rr.Code, rr.Body.String())
			}
			var refusal map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &refusal); err != nil || len(refusal) != 1 || !strings.Contains(refusal["error"], tc.field) {
				t.Errorf("refusal %s (%v), want {\"error\": ...} naming %s", rr.Body.String(), err, tc.field)
			}
			if v := cfgStore.Get().Voice; !v.Enabled || v.Engine != "dgagent" || v.TTSEngine != "aura" {
				t.Errorf("a refused PATCH changed the voice: enabled %v, engine %q, tts_engine %q; want true, dgagent, aura", v.Enabled, v.Engine, v.TTSEngine)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
				t.Errorf("a refused PATCH rewrote the config file (%v):\n%s", err, after)
			}
			if n := announced.Load(); n != 0 {
				t.Errorf("a refused PATCH announced %d config changes", n)
			}
		})
	}
}

// TestOpenAPISpecDocumentsTheVoiceSettings fails if the OpenAPI spec of GET
// and PATCH /api/config does not give voice.enabled as a boolean and the
// values voice.engine and voice.tts_engine accept.
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
		voice := dig(t, spec, append(slices.Clone(schema), "properties", "voice", "properties")...)
		if typ := dig(t, voice, "enabled", "type"); typ != "boolean" {
			t.Errorf("%v voice.enabled type %v, want boolean", schema[:3], typ)
		}
		for field, want := range map[string][]any{
			"engine":     {"tts", "gptlive", "dgagent"},
			"tts_engine": {"gemini", "aura"},
		} {
			if got := dig(t, voice, field, "enum"); !reflect.DeepEqual(got, want) {
				t.Errorf("%v voice.%s enum %v, want %v", schema[:3], field, got, want)
			}
		}
	}
}
