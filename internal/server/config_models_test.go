package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestGetConfigReportsTheModelOfEveryRole: GET /api/config reports the model
// the running process uses for each role, live and batch transcription,
// summaries, embeddings and the voice, as the process resolved it (an
// environment override included), so the slice 2 acceptance check compares
// what runs, not the YAML file on disk.
func TestGetConfigReportsTheModelOfEveryRole(t *testing.T) {
	newTestConfigStore(t) // clears the environment
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte(`deepgram_model: nova-3
batch_transcription:
  provider: deepgram
  model: nova-3
summarization:
  model: gemini/gemini-4-carbon
embedding_model: gemini/gemini-embedding-2
genai_backend: vertex
gcp_project: theorem-agent-infra
gcp_location: global
tts_provider: gemini
tts_model: gemini-3.8-flash-tts
tts_voice: Kore
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvPrefix+"EMBEDDING_MODEL", "gemini/overridden-embedding")
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
		Models map[string]string `json:"models"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"live_transcription":           "nova-3",
		"batch_transcription_provider": "deepgram",
		"batch_transcription":          "nova-3",
		"summarization":                "gemini/gemini-4-carbon",
		"embedding":                    "gemini/overridden-embedding",
		"genai_backend":                "vertex",
		"gcp_project":                  "theorem-agent-infra",
		"gcp_location":                 "global",
		"tts_provider":                 "gemini",
		"tts":                          "gemini-3.8-flash-tts",
		"tts_voice":                    "Kore",
	}
	for role, model := range want {
		if got := resp.Models[role]; got != model {
			t.Errorf("models.%s = %q, want %q", role, got, model)
		}
	}
}
