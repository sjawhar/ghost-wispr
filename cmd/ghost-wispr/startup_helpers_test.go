package main

import (
	"errors"
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

// ttsConfig is a config whose tts role is model on provider p, named name,
// which signs in with key when it signs in with one.
func ttsConfig(name string, p *config.Provider, model, key string) config.Config {
	cfg := config.Config{Providers: map[string]config.Provider{name: *p}, TTS: config.TTS{Model: model}}
	if key != "" {
		cfg.ProviderKeys = map[string]string{p.Auth.Env: key}
	}
	return cfg
}

// TestNewSpeechProviderBuildsTheNamedProvider fails if the speak API's
// provider is not the one tts.model names, or one that cannot speak is not
// refused with its reason.
func TestNewSpeechProviderBuildsTheNamedProvider(t *testing.T) {
	keyed := func(api string) *config.Provider {
		return &config.Provider{API: api, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_TTS_KEY"}}
	}
	for _, tc := range []struct {
		name  string
		cfg   config.Config
		built bool   // the case wants a provider built, not an error
		want  string // the provider's name, or a part of the error
	}{
		{"elevenlabs", ttsConfig("eleven", keyed(config.APIElevenLabs), "eleven/eleven_flash_v2_5", "k-1"), true, "eleven"},
		{"aura", ttsConfig("dg", keyed(config.APIDeepgram), "dg/aura-2-thalia-en", "k-1"), true, "dg"},
		{"openai", ttsConfig("oa", keyed(config.APIOpenAI), "oa/gpt-4o-mini-tts", "k-1"), true, "oa"},
		{"no such provider", ttsConfig("dg", keyed(config.APIDeepgram), "nowhere/aura", "k-1"), false, `"nowhere"`},
		{"a provider with no speech", ttsConfig("claude", keyed(config.APIAnthropic), "claude/x", "k-1"), false, "anthropic"},
		{"vertex with no project", ttsConfig("vertex", &config.Provider{API: config.APIVertex, Auth: config.ProviderAuth{Kind: config.AuthGoogle}}, "vertex/gemini-tts", ""), false, "project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := newSpeechProvider(&tc.cfg)
			if tc.built {
				if err != nil || provider.Name() != tc.want {
					t.Fatalf("newSpeechProvider = %v, %v; want the provider %q", provider, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newSpeechProvider = %v, %v; want an error containing %q", provider, err, tc.want)
			}
		})
	}
}

// TestBatchClientConfigUsesTheSummaryProvider fails if the summary batch
// client is not configured from summarization.model's provider, or a
// provider that takes no batch job (Vertex AI, to which genai sends no batch
// of inlined requests, or another API) is not refused with
// errNoBatchProvider.
func TestBatchClientConfigUsesTheSummaryProvider(t *testing.T) {
	gemini := geminiKeyConfig("k-1")
	gemini.Summarization.Model = "gemini/gemini-test"
	clientConfig, model, err := batchClientConfig(&gemini)
	if err != nil || clientConfig.Backend != genai.BackendGeminiAPI || model != "gemini-test" {
		t.Fatalf("batchClientConfig on the Gemini API = %+v, %q, %v; want the Gemini API and the model", clientConfig, model, err)
	}
	vertex := config.Config{
		Providers:     map[string]config.Provider{"vertex": {API: config.APIVertex, Project: "project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}}},
		Summarization: config.Summarization{Model: "vertex/gemini-test"},
	}
	openai := config.Config{
		Providers:     map[string]config.Provider{"gateway": {API: config.APIOpenAI, URL: "https://gateway.example.test/v1", Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "aud"}}},
		Summarization: config.Summarization{Model: "gateway/gpt-test"},
	}
	for name, cfg := range map[string]*config.Config{"Vertex AI": &vertex, "an OpenAI API provider": &openai} {
		if _, _, err := batchClientConfig(cfg); !errors.Is(err, errNoBatchProvider) {
			t.Errorf("batchClientConfig on %s = %v, want %v", name, err, errNoBatchProvider)
		}
	}
}
