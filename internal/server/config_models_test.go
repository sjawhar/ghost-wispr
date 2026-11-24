package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// providersYAML is a unit's ghost-wispr.yaml with a provider of each sign-in
// kind and every role set: the model gateway, Vertex AI in two locations with
// billing labels, OpenAI by workload identity and Deepgram by key.
const providersYAML = `providers:
  gateway: {api: openai, url: https://gateway.example.test/v1, auth: {kind: google_id_token, audience: example-gateway}}
  vertex: {api: vertex, project: example-project, location: global, auth: {kind: google}, labels: {owner: example-owner, stream: example-stream}}
  vertex-us: {api: vertex, project: example-project, location: us, auth: {kind: google}, labels: {owner: example-owner, stream: example-stream}}
  openai: {api: openai, auth: {kind: openai_wif, identity_provider_id: idp-example, service_account_id: sa-example}}
  deepgram: {api: deepgram, auth: {kind: api_key, env: GHOST_WISPR_DEEPGRAM_API_KEY}}
transcription: {model: deepgram/nova-3}
batch_transcription: {model: deepgram/nova-3-general}
summarization: {model: vertex/gemini-4-carbon}
embedding: {model: gateway/gemini-embedding-2}
concierge: {model: vertex/gemini-3.5-flash-lite}
tts: {model: vertex-us/gemini-3.8-flash-tts, voice: Kore}
voice:
  engine: gptlive
  gptlive: {model: openai/gpt-live-1}
  dgagent: {provider: deepgram}
`

// writeProvidersConfig clears every GHOST_WISPR_ variable and writes
// providersYAML to a config file, whose path it returns.
func writeProvidersConfig(t *testing.T) string {
	t.Helper()
	newTestConfigStore(t) // clears the environment
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte(providersYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// newProvidersConfigStore returns a config store read from providersYAML,
// with no GHOST_WISPR_ variable set, and the path of its file.
func newProvidersConfigStore(t *testing.T) (*config.Store, string) {
	t.Helper()
	path := writeProvidersConfig(t)
	cfgStore, _, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return cfgStore, path
}

func newConfigHandler(t *testing.T, cfgStore *config.Store, controls *ControlHooks) http.Handler {
	t.Helper()
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, controls, "", nil, cfgStore)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return h
}

// TestGetConfigReportsTheModelOfEveryRole fails if GET /api/config does not
// report, under models, exactly the provider/model of each role as the
// running process resolved it (an environment override included), so an
// acceptance check compares what runs, not the YAML file on disk; or if it
// keeps a key of the settings providers replaced, such as genai_backend.
func TestGetConfigReportsTheModelOfEveryRole(t *testing.T) {
	path := writeProvidersConfig(t)
	t.Setenv(config.EnvPrefix+"EMBEDDING_MODEL", "gateway/overridden-embedding")
	cfgStore, _, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h := newConfigHandler(t, cfgStore, &ControlHooks{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Models map[string]string `json:"models"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"live_transcription":  "deepgram/nova-3",
		"batch_transcription": "deepgram/nova-3-general",
		"summarization":       "vertex/gemini-4-carbon",
		"embedding":           "gateway/overridden-embedding",
		"concierge":           "vertex/gemini-3.5-flash-lite",
		"tts":                 "vertex-us/gemini-3.8-flash-tts",
		"tts_voice":           "Kore",
		"gptlive":             "openai/gpt-live-1",
		"dgagent_provider":    "deepgram",
	}
	if !reflect.DeepEqual(resp.Models, want) {
		t.Errorf("models = %v,\nwant exactly %v", resp.Models, want)
	}
}
