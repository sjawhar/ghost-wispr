package llm

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// exampleLabels are a provider's request labels in the tests.
var exampleLabels = map[string]string{"owner": "example-owner", "stream": "example-stream"}

// generateBodies is a fake Gemini endpoint: it answers every generateContent
// with a JSON summary and keeps each request body's labels, and whether it
// had any.
type generateBodies struct {
	mu     sync.Mutex
	labels []map[string]string
	has    []bool
}

func (g *generateBodies) serve(t *testing.T) *httptest.Server {
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
		g.mu.Lock()
		g.labels, g.has = append(g.labels, labels), append(g.has, has)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"title\":\"t\",\"summary\":\"s\"}"}]}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// completeBoth makes one Complete and one CompleteJSON of client.
func completeBoth(t *testing.T, client Client) {
	t.Helper()
	messages := []Message{{Role: "system", Content: "summarize"}, {Role: "user", Content: "the transcript"}}
	if _, err := client.Complete(context.Background(), messages); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := client.CompleteJSON(context.Background(), messages, map[string]any{"type": "object"}); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
}

// TestSummariesOnVertexAICarryTheProvidersLabels fails if a summary's
// generateContent on a Vertex AI provider (Complete, and CompleteJSON, which
// summaries, titles and speaker names use) does not carry the provider's
// labels, which Vertex AI copies into Cloud Billing.
func TestSummariesOnVertexAICarryTheProvidersLabels(t *testing.T) {
	var bodies generateBodies
	srv := bodies.serve(t)
	p := testProvider(t, &config.Provider{API: config.APIVertex, URL: srv.URL, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}, Labels: exampleLabels}, providers.Options{
		GoogleCredentials: func(context.Context, string, ...string) (*auth.Credentials, error) {
			return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken("tok-1")}), nil
		},
	})
	client, err := NewClient(p, "gemini-x")
	if err != nil {
		t.Fatal(err)
	}
	completeBoth(t, client)
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

// TestSummariesOnTheGeminiAPICarryNoLabels fails if a summary's
// generateContent on a Gemini API provider, such as a model gateway's Google
// routes, carries labels, even when its entry names some: only Vertex AI
// takes them.
func TestSummariesOnTheGeminiAPICarryNoLabels(t *testing.T) {
	var bodies generateBodies
	srv := bodies.serve(t)
	p := testProvider(t, &config.Provider{API: config.APIGemini, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}, Labels: exampleLabels}, providers.Options{})
	client, err := NewClient(p, "gemini-x")
	if err != nil {
		t.Fatal(err)
	}
	completeBoth(t, client)
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
