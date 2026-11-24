package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// TestVertexAIEmbeddingsCarryNoLabels fails if an embedding call on a Vertex
// AI provider whose entry names labels carries them, at any depth of its
// body: the labels are for generate calls (generateContent and
// streamGenerateContent) alone.
func TestVertexAIEmbeddingsCarryNoLabels(t *testing.T) {
	var rec recorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": map[string]any{"values": []float32{1}}})
	}))
	defer srv.Close()
	p := testProvider(t, &config.Provider{API: config.APIVertex, URL: srv.URL, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}, Labels: map[string]string{"owner": "example-owner"}}, providers.Options{
		GoogleCredentials: func(context.Context, string, ...string) (*auth.Credentials, error) {
			return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken("tok-1")}), nil
		},
	})
	client, err := NewClient(p, "gemini-embedding-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Embed(context.Background(), []string{"a question"}, TaskTypeQuery); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	requests := rec.all()
	if len(requests) != 1 {
		t.Fatalf("Vertex AI got %d requests, want 1", len(requests))
	}
	if body := string(requests[0].body); !strings.Contains(body, "a question") || strings.Contains(body, `"labels"`) || strings.Contains(body, "example-owner") {
		t.Fatalf("the embedding request %s carries labels, or not the text", body)
	}
}
