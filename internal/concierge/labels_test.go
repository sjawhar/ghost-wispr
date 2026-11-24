package concierge

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// exampleLabels are a provider's request labels in the tests.
var exampleLabels = map[string]string{"owner": "example-owner", "stream": "example-stream"}

// streamBodies is a fake Gemini endpoint: it streams "ok" to every
// streamGenerateContent and keeps each request body's labels, and whether it
// had any.
type streamBodies struct {
	mu     sync.Mutex
	labels []map[string]string
	has    []bool
}

func (s *streamBodies) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body %s: %v", raw, err)
		}
		var labels map[string]string
		_, has := body["labels"]
		if has {
			_ = json.Unmarshal(body["labels"], &labels)
		}
		s.mu.Lock()
		s.labels, s.has = append(s.labels, labels), append(s.has, has)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}],\"role\":\"model\"}}]}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// streamAll streams one answer of generator for each config, and fails the
// test on an error.
func streamAll(t *testing.T, generator Generator, configs ...*genai.GenerateContentConfig) {
	t.Helper()
	for _, cfg := range configs {
		for _, err := range generator.GenerateContentStream(context.Background(), "gemini-x", genai.Text("ping"), cfg) {
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
		}
	}
}

// TestTheConciergeOnVertexAICarriesTheProvidersLabels fails if a
// streamGenerateContent of the Concierge's model client on a Vertex AI
// provider (each round of an answer, and the ear rewrite) does not carry the
// provider's labels, which Vertex AI copies into Cloud Billing, or if adding
// them changes the config the Concierge passed, which it reuses.
func TestTheConciergeOnVertexAICarriesTheProvidersLabels(t *testing.T) {
	var bodies streamBodies
	srv := bodies.serve(t)
	p := generatorProvider(t, &config.Provider{API: config.APIVertex, URL: srv.URL, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}, Labels: exampleLabels})
	generator, err := NewGenerator(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	passed := &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText("answer", genai.RoleUser)}
	streamAll(t, generator, passed, nil)
	if passed.Labels != nil {
		t.Errorf("the config the Concierge passed now holds labels %v", passed.Labels)
	}
	bodies.mu.Lock()
	defer bodies.mu.Unlock()
	if len(bodies.labels) != 2 {
		t.Fatalf("Vertex AI got %d requests, want 2", len(bodies.labels))
	}
	for i, got := range bodies.labels {
		if !maps.Equal(got, exampleLabels) {
			t.Errorf("request %d carried labels %v, want the provider's %v", i, got, exampleLabels)
		}
	}
}

// TestTheConciergeOnTheGeminiAPICarriesNoLabels fails if a
// streamGenerateContent of the Concierge's model client on a Gemini API
// provider, such as a model gateway's Google routes, carries labels, even
// when its entry names some: only Vertex AI takes them.
func TestTheConciergeOnTheGeminiAPICarriesNoLabels(t *testing.T) {
	var bodies streamBodies
	srv := bodies.serve(t)
	p, err := providers.New(&config.Config{
		Providers:    map[string]config.Provider{"p": {API: config.APIGemini, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}, Labels: exampleLabels}},
		ProviderKeys: map[string]string{"EXAMPLE_KEY": "sk-1"},
	}, providers.Options{}).Get("p")
	if err != nil {
		t.Fatal(err)
	}
	generator, err := NewGenerator(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	streamAll(t, generator, &genai.GenerateContentConfig{}, nil)
	bodies.mu.Lock()
	defer bodies.mu.Unlock()
	if len(bodies.has) != 2 {
		t.Fatalf("the Gemini API got %d requests, want 2", len(bodies.has))
	}
	for i, has := range bodies.has {
		if has {
			t.Errorf("request %d carried labels %v, want none", i, bodies.labels[i])
		}
	}
}

// TestTheConciergeOnAGatewayCarriesNoLabels fails if a Concierge answer on
// an OpenAI API provider, such as the model gateway, fails or sends labels
// when its entry names some: its chat completions have no place for them,
// and the client refuses a request it cannot translate.
func TestTheConciergeOnAGatewayCarriesNoLabels(t *testing.T) {
	srv := newChatServer(t, []string{textEvent("ok"), finishEvent("stop")})
	mint := func(context.Context, string, string) (string, error) { return "id-1", nil }
	p, err := providers.New(&config.Config{Providers: map[string]config.Provider{
		"gateway": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}, Labels: exampleLabels},
	}}, providers.Options{IDToken: mint}).Get("gateway")
	if err != nil {
		t.Fatal(err)
	}
	generator, err := NewGenerator(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	streamAll(t, generator, &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText("answer", genai.RoleUser)})
	requests := srv.requests()
	if len(requests) != 1 {
		t.Fatalf("the gateway got %d requests, want 1", len(requests))
	}
	for key := range requests[0] {
		if strings.Contains(key, "label") {
			t.Errorf("the request carried %s: %v", key, requests[0])
		}
	}
}
