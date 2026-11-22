package main

import (
	"github.com/sjawhar/ghost-wispr/internal/config"
)

// exampleGeminiKeyEnv is the variable the test configs' Gemini API provider
// reads its key from.
const exampleGeminiKeyEnv = "EXAMPLE_GEMINI_KEY"

// geminiKeyConfig is a config whose Concierge runs on a Gemini API provider
// that signs in with key; "" sets no key.
func geminiKeyConfig(key string) config.Config {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"gemini": {API: config.APIGemini, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: exampleGeminiKeyEnv}},
		},
		Concierge: config.Concierge{Model: "gemini/gemini-test"},
	}
	if key != "" {
		cfg.ProviderKeys = map[string]string{exampleGeminiKeyEnv: key}
	}
	return cfg
}

// deepgramKeyConfig is a config whose live transcription runs on Deepgram's
// nova-3 with key; "" sets no key.
func deepgramKeyConfig(key string) config.Config {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"deepgram": {API: config.APIDeepgram, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: config.EnvPrefix + "DEEPGRAM_API_KEY"}},
		},
		Transcription: config.Transcription{Model: "deepgram/nova-3"},
		Voice:         config.Voice{DGAgent: config.DGAgent{Provider: "deepgram"}},
	}
	if key != "" {
		cfg.ProviderKeys = map[string]string{config.EnvPrefix + "DEEPGRAM_API_KEY": key}
	}
	return cfg
}
