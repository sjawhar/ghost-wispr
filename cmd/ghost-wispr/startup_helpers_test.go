package main

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/tts"
)

// TestTTSStatusResponseNamesEveryField fails if the TTS routes' status
// response drops, renames or mixes up a field of the request's status.
func TestTTSStatusResponseNamesEveryField(t *testing.T) {
	status := tts.SpeakStatus{ID: "id", Status: "done", BytesWritten: 3, DurationMs: 4, Model: "model", Voice: "voice", Error: "error"}
	want := map[string]any{
		"id": "id", "status": "done", "bytes_written": int64(3), "duration_ms": int64(4),
		"model": "model", "voice": "voice", "error": "error",
	}
	if got := ttsStatusResponse(&status); !reflect.DeepEqual(got, want) {
		t.Fatalf("ttsStatusResponse = %v, want %v", got, want)
	}
}

// TestNewTTSProviderBuildsTheNamedProvider fails if a configured TTS provider
// is not the one built, or a provider that cannot be built is not refused
// with its reason.
func TestNewTTSProviderBuildsTheNamedProvider(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-credentials.json")
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string // the provider's name, or the start of the error
	}{
		{"elevenlabs", config.Config{TTSProvider: "elevenlabs", TTSAPIKey: "key"}, "elevenlabs"},
		{"elevenlabs without a key", config.Config{TTSProvider: "elevenlabs"}, "ElevenLabs requires GHOST_WISPR_TTS_API_KEY"},
		{"google with a key", config.Config{TTSProvider: "google", TTSAPIKey: "key"}, "google"},
		{"google without credentials", config.Config{TTSProvider: "google", GoogleCredentialsFile: missing}, "detect Google credentials"},
		{"gemini without credentials", config.Config{TTSProvider: "gemini", GCPProject: "project", GoogleCredentialsFile: missing}, "detect Google credentials"},
		{"unsupported", config.Config{TTSProvider: "other"}, "unsupported TTS provider: other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := newTTSProvider(context.Background(), &tc.cfg)
			got := ""
			if err != nil {
				got = err.Error()
			} else {
				got = provider.Name()
			}
			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("newTTSProvider = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBatchClientConfigUsesTheGeminiSettings fails if the summary batch
// client is not configured from the Gemini settings, or a Vertex AI backend
// is configured without detecting its credentials.
func TestBatchClientConfigUsesTheGeminiSettings(t *testing.T) {
	clientConfig, err := batchClientConfig(context.Background(), &config.Config{GenAIBackend: "gemini", GeminiAPIKey: "key"})
	if err != nil || clientConfig.APIKey != "key" || clientConfig.Backend != genai.BackendGeminiAPI {
		t.Fatalf("batchClientConfig with a Gemini key = %+v, %v; want the Gemini API with that key", clientConfig, err)
	}
	if _, err := batchClientConfig(context.Background(), &config.Config{GenAIBackend: "gemini"}); err == nil ||
		err.Error() != "genai backend not configured for batch operations: gemini api key is required" {
		t.Fatalf("batchClientConfig without a Gemini key = %v, want the batch configuration error", err)
	}
	missing := filepath.Join(t.TempDir(), "missing-credentials.json")
	if _, err := batchClientConfig(context.Background(), &config.Config{GenAIBackend: "vertex", GCPProject: "project", GoogleCredentialsFile: missing}); err == nil ||
		!strings.HasPrefix(err.Error(), "detect Google credentials") {
		t.Fatalf("batchClientConfig for Vertex AI without credentials = %v, want the credential error", err)
	}
}
