package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/googleauthtest"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

const vertexTestToken = "x509-" + "federated-" + "token"

type vertexRequestRecorder struct {
	mu            sync.Mutex
	authorization []string
}

func (r *vertexRequestRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		r.mu.Lock()
		r.authorization = append(r.authorization, request.Header.Get("Authorization"))
		r.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// vertexTestConfig is a config whose vertex provider is the test server,
// signed in by an X.509 workload identity credential file whose token is
// token, with summaries on it.
func vertexTestConfig(t *testing.T, url, token string) *config.Config {
	t.Helper()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	return &config.Config{
		GoogleCredentialsFile: googleauthtest.X509CredentialFile(t, token),
		Providers: map[string]config.Provider{
			"vertex": {API: config.APIVertex, URL: url, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
		},
		Summarization: config.Summarization{Model: "vertex/gemini-x"},
	}
}

// takeAuthorization returns the Authorization headers the server saw since
// the last call.
func (r *vertexRequestRecorder) takeAuthorization() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := r.authorization
	r.authorization = nil
	return seen
}

func carries(seen []string, token string) bool {
	for _, authorization := range seen {
		if authorization == "Bearer "+token {
			return true
		}
	}
	return false
}

// TestVertexUsesConfiguredCredentials fails if a summary, an embedding or a
// genai call (the Concierge's) on a Vertex AI provider does not carry the
// token of the credential file google_credentials_file names, the office
// Pi's X.509 workload identity, or if a Vertex AI provider is taken for a
// summary batch provider: genai sends it no batch of inlined requests.
func TestVertexUsesConfiguredCredentials(t *testing.T) {
	recorder := &vertexRequestRecorder{}
	server := recorder.server(t)
	cfg := vertexTestConfig(t, server.URL, vertexTestToken)
	p, err := unitProviders.of(cfg).Get("vertex")
	if err != nil {
		t.Fatal(err)
	}

	summaryClient, err := llm.NewClient(p, "gemini-x")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = summaryClient.Complete(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
	if seen := recorder.takeAuthorization(); !carries(seen, vertexTestToken) {
		t.Fatalf("a summary carried %q, want the configured credential's token", seen)
	}

	embeddingClient, err := embedding.NewClient(p, "gemini-embedding-2")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = embeddingClient.Embed(context.Background(), []string{"hi"}, embedding.TaskTypeDocument)
	if seen := recorder.takeAuthorization(); !carries(seen, vertexTestToken) {
		t.Fatalf("an embedding carried %q, want the configured credential's token", seen)
	}

	genaiConfig, err := p.GenAIConfig()
	if err != nil {
		t.Fatal(err)
	}
	genaiClient, err := genai.NewClient(context.Background(), genaiConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = genaiClient.Models.GenerateContent(context.Background(), "gemini-x", genai.Text("hi"), nil)
	if seen := recorder.takeAuthorization(); !carries(seen, vertexTestToken) {
		t.Fatalf("a genai call carried %q, want the configured credential's token", seen)
	}

	if _, _, err := batchClientConfig(cfg); !errors.Is(err, errNoBatchProvider) {
		t.Fatalf("batchClientConfig on Vertex AI = %v, want %v", err, errNoBatchProvider)
	}
}

// TestVertexCredentialsFollowTheConfig fails if two clients of one config do
// not share the credentials detected for its file, or if a settings change of
// google_credentials_file leaves requests on the old file's token.
func TestVertexCredentialsFollowTheConfig(t *testing.T) {
	recorder := &vertexRequestRecorder{}
	server := recorder.server(t)
	first := vertexTestConfig(t, server.URL, vertexTestToken)
	store, _, err := config.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) {
		cfg.GoogleCredentialsFile, cfg.Providers, cfg.Summarization.Model = first.GoogleCredentialsFile, first.Providers, first.Summarization.Model
	}); err != nil {
		t.Fatal(err)
	}
	current := store.Get()
	one, _ := unitProviders.of(&current).Get("vertex")
	again := store.Get()
	two, _ := unitProviders.of(&again).Get("vertex")
	oneCreds, err := one.GoogleCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	twoCreds, err := two.GoogleCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if oneCreds != twoCreds {
		t.Fatal("two reads of one config detected the credentials twice")
	}

	secondToken := "second-" + "federated-" + "token"
	secondPath := googleauthtest.X509CredentialFile(t, secondToken)
	if err := store.Update(func(cfg *config.Config) { cfg.GoogleCredentialsFile = secondPath }); err != nil {
		t.Fatal(err)
	}
	summaryClient, err := summaryClients(store)("vertex", "gemini-x")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = summaryClient.Complete(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
	if seen := recorder.takeAuthorization(); len(seen) == 0 || seen[0] != "Bearer "+secondToken {
		t.Fatalf("after the credential file changed, Vertex requests carried %q, want the second file's token", seen)
	}
}
