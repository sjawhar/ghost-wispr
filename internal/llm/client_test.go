package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// testProvider is entry as the provider "p", signed in with opts; an api_key
// entry reads the key "sk-1" from EXAMPLE_KEY.
func testProvider(t *testing.T, entry *config.Provider, opts providers.Options) *providers.Provider {
	t.Helper()
	cfg := &config.Config{
		Providers:    map[string]config.Provider{"p": *entry},
		ProviderKeys: map[string]string{"EXAMPLE_KEY": "sk-1"},
	}
	p, err := providers.New(cfg, opts).Get("p")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// keyProvider is a provider of api at url that signs in with the key "sk-1".
func keyProvider(t *testing.T, api, url string) *providers.Provider {
	t.Helper()
	return testProvider(t, &config.Provider{API: api, URL: url, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}}, providers.Options{})
}

// idTokens mints the Google ID token "id-1" and records each audience it is
// asked for, as a model gateway's sign-in does.
type idTokens struct {
	mu        sync.Mutex
	audiences []string
}

func (m *idTokens) mint(_ context.Context, _, audience string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audiences = append(m.audiences, audience)
	return "id-1", nil
}

// gatewayProvider is a provider of api at url signed in by a Google ID token
// for the audience example-gateway, minted by m.
func gatewayProvider(t *testing.T, api, url string, m *idTokens) *providers.Provider {
	t.Helper()
	return testProvider(t, &config.Provider{API: api, URL: url, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}}, providers.Options{IDToken: m.mint})
}

// staticToken is a Google access token that never changes.
type staticToken string

func (s staticToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: string(s), Type: "Bearer"}, nil
}

// vertexProvider is a Vertex AI provider of example-project in location,
// served at url, signed in by Google credentials whose token is "tok-1".
func vertexProvider(t *testing.T, url, location string) *providers.Provider {
	t.Helper()
	return testProvider(t, &config.Provider{API: config.APIVertex, URL: url, Project: "example-project", Location: location, Auth: config.ProviderAuth{Kind: config.AuthGoogle}}, providers.Options{
		GoogleCredentials: func(context.Context, string, ...string) (*auth.Credentials, error) {
			return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken("tok-1")}), nil
		},
	})
}

// seen is what a fake provider read of the one request it was sent.
type seen struct {
	mu     sync.Mutex
	path   string
	header http.Header
}

func (s *seen) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path, s.header = r.URL.Path, r.Header.Clone()
}

func (s *seen) get() (string, http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path, s.header
}

func chatCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "created": 123, "model": "gpt-test",
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
	})
}

func anthropicMessage(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-test",
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
}

func geminiAnswer(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"candidates": []map[string]any{{"content": map[string]any{"parts": []map[string]any{{"text": text}}, "role": "model"}}},
	})
}

// TestNewClientRefusesAnAPIWithNoTextClient fails if a provider whose API
// has no chat client (Deepgram's) gets a client, or the refusal does not
// name the API.
func TestNewClientRefusesAnAPIWithNoTextClient(t *testing.T) {
	client, err := NewClient(keyProvider(t, config.APIDeepgram, ""), "nova-3")
	if err == nil || client != nil {
		t.Fatalf("NewClient on a deepgram provider = %v, %v; want a refusal", client, err)
	}
	if !strings.Contains(err.Error(), config.APIDeepgram) {
		t.Fatalf("the refusal %q does not name the provider's API", err)
	}
}

// TestAnOpenAIClientSendsToTheProviderWithItsCredential fails if an OpenAI
// API client does not send its chat completion to the provider's url, or
// sends it without the provider's credential: here a model gateway's Google
// ID token, minted for the provider's audience.
func TestAnOpenAIClientSendsToTheProviderWithItsCredential(t *testing.T) {
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		chatCompletion(w, "ok")
	}))
	defer srv.Close()
	m := &idTokens{}
	client, err := NewClient(gatewayProvider(t, config.APIOpenAI, srv.URL+"/v1", m), "openai/gpt-test")
	if err != nil {
		t.Fatal(err)
	}

	answer, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "ping"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	path, header := got.get()
	if answer != "ok" || path != "/v1/chat/completions" {
		t.Fatalf("answer %q from path %q; want ok from /v1/chat/completions", answer, path)
	}
	if auth := header.Get("Authorization"); auth != "Bearer id-1" {
		t.Fatalf("Authorization = %q, want the provider's ID token", auth)
	}
	if len(m.audiences) != 1 || m.audiences[0] != "example-gateway" {
		t.Fatalf("ID tokens minted for %v, want one for example-gateway", m.audiences)
	}
}

// TestAnAnthropicClientReachesTheProviderURL fails if an Anthropic API client
// sends its message anywhere but the provider's url, or without the
// provider's credential in the header its sign-in puts it: a token as a
// bearer token (no placeholder key beside it), a key as x-api-key.
func TestAnAnthropicClientReachesTheProviderURL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider func(t *testing.T, url string) *providers.Provider
		header   string
		want     string
	}{
		{
			name: "google_id_token",
			provider: func(t *testing.T, url string) *providers.Provider {
				return gatewayProvider(t, config.APIAnthropic, url, &idTokens{})
			},
			header: "Authorization", want: "Bearer id-1",
		},
		{
			name:     "api_key",
			provider: func(t *testing.T, url string) *providers.Provider { return keyProvider(t, config.APIAnthropic, url) },
			header:   "X-Api-Key", want: "sk-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got seen
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.record(r)
				anthropicMessage(w, "ok")
			}))
			defer srv.Close()
			client, err := NewClient(tc.provider(t, srv.URL), "claude-test")
			if err != nil {
				t.Fatal(err)
			}

			answer, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "ping"}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			path, header := got.get()
			if answer != "ok" || path != "/v1/messages" {
				t.Fatalf("answer %q from path %q; want ok from /v1/messages", answer, path)
			}
			if v := header.Get(tc.header); v != tc.want {
				t.Fatalf("%s = %q, want %q", tc.header, v, tc.want)
			}
			if tc.header == "Authorization" && header.Get("X-Api-Key") != "" {
				t.Fatalf("x-api-key %q sent beside the bearer token", header.Get("X-Api-Key"))
			}
		})
	}
}

// TestAVertexClientBuildsTheVertexPath fails if a Vertex AI client does not
// ask for the model in the provider's project and location, or sends the
// request without the provider's Google access token.
func TestAVertexClientBuildsTheVertexPath(t *testing.T) {
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		geminiAnswer(w, "ok")
	}))
	defer srv.Close()
	client, err := NewClient(vertexProvider(t, srv.URL, "us-central1"), "gemini-x")
	if err != nil {
		t.Fatal(err)
	}

	answer, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "ping"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	path, header := got.get()
	if want := "/projects/example-project/locations/us-central1/publishers/google/models/gemini-x:generateContent"; answer != "ok" || !strings.HasSuffix(path, want) {
		t.Fatalf("answer %q from path %q; want ok from a path ending %s", answer, path, want)
	}
	if auth := header.Get("Authorization"); auth != "Bearer tok-1" {
		t.Fatalf("Authorization = %q, want the provider's Google access token", auth)
	}
}

// TestAGeminiAPIClientSendsTheProviderKey fails if a Gemini API client does
// not ask the provider's url for the model, or sends genai's placeholder in
// place of the provider's key.
func TestAGeminiAPIClientSendsTheProviderKey(t *testing.T) {
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		geminiAnswer(w, "ok")
	}))
	defer srv.Close()
	client, err := NewClient(keyProvider(t, config.APIGemini, srv.URL), "gemini-x")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "ping"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	path, header := got.get()
	if !strings.HasSuffix(path, "/models/gemini-x:generateContent") || strings.Contains(path, "/projects/") {
		t.Fatalf("path %q, want the Gemini API's generateContent of gemini-x", path)
	}
	if key := header.Get("X-Goog-Api-Key"); key != "sk-1" {
		t.Fatalf("x-goog-api-key = %q, want the provider's key", key)
	}
}
