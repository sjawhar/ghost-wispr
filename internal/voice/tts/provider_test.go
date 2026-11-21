package tts

import (
	"context"
	"testing"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// The key every api_key provider of a test signs in with, from testKeyEnv.
const (
	testKeyEnv = "EXAMPLE_TTS_KEY"
	testKey    = "k-1"
)

// keyAuth signs in with testKey.
func keyAuth() config.ProviderAuth {
	return config.ProviderAuth{Kind: config.AuthAPIKey, Env: testKeyEnv}
}

// googleAuth signs in with the Google credentials testProvider is given.
func googleAuth() config.ProviderAuth { return config.ProviderAuth{Kind: config.AuthGoogle} }

// testProvider is p, signed in as a unit signs in: an api_key provider with
// testKey, a google one with creds (testCredentials when nil).
func testProvider(t testing.TB, p *config.Provider, creds *auth.Credentials) *providers.Provider {
	t.Helper()
	if creds == nil {
		creds = testCredentials()
	}
	cfg := &config.Config{
		Providers:    map[string]config.Provider{"tts": *p},
		ProviderKeys: map[string]string{testKeyEnv: testKey},
	}
	set := providers.New(cfg, providers.Options{
		GoogleCredentials: func(context.Context, string, ...string) (*auth.Credentials, error) { return creds, nil },
	})
	got, err := set.Get("tts")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// vertexAt is a Vertex AI provider of example-project in the us
// multi-region, whose url is url ("" for the location's host).
func vertexAt(url string) *config.Provider {
	return &config.Provider{API: config.APIVertex, URL: url, Project: "example-project", Location: "us", Auth: googleAuth()}
}

// newTestGemini is the engine of Gemini TTS on the Vertex AI provider at
// url, signed in with creds (testCredentials when nil).
func newTestGemini(t testing.TB, url string, creds *auth.Credentials, opts ...Option) Engine {
	t.Helper()
	e, err := NewEngine(testProvider(t, vertexAt(url), creds), "gemini-3.8-flash-tts", "", opts...)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}
