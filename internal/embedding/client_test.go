package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/retry"
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

// gatewayProvider is an OpenAI API provider at url signed in by the Google ID
// token "id-1" for the audience example-gateway, as a model gateway is.
func gatewayProvider(t *testing.T, url string) *providers.Provider {
	t.Helper()
	mint := func(context.Context, string, string) (string, error) { return "id-1", nil }
	return testProvider(t, &config.Provider{API: config.APIOpenAI, URL: url, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}}, providers.Options{IDToken: mint})
}

// staticToken is a Google access token that never changes.
type staticToken string

func (s staticToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: string(s), Type: "Bearer"}, nil
}

// vertexProvider is a Vertex AI provider of example-project at global,
// served at url, signed in by Google credentials whose token is "tok-1".
func vertexProvider(t *testing.T, url string) *providers.Provider {
	t.Helper()
	return testProvider(t, &config.Provider{API: config.APIVertex, URL: url, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}}, providers.Options{
		GoogleCredentials: func(context.Context, string, ...string) (*auth.Credentials, error) {
			return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken("tok-1")}), nil
		},
	})
}

// request is what a fake provider read of one request it was sent.
type request struct {
	path   string
	header http.Header
	body   []byte
}

// recorder keeps the requests a fake provider was sent, in order.
type recorder struct {
	mu       sync.Mutex
	requests []request
}

// add records r and returns how many requests came before it.
func (rec *recorder) add(r *http.Request) int {
	body, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.requests = append(rec.requests, request{path: r.URL.Path, header: r.Header.Clone(), body: body})
	return len(rec.requests) - 1
}

func (rec *recorder) all() []request {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]request(nil), rec.requests...)
}

// openAIEmbedding answers one OpenAI embeddings request with vector.
func openAIEmbedding(w http.ResponseWriter, vector []float32) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list", "model": "gemini-embedding-2",
		"data":  []map[string]any{{"object": "embedding", "index": 0, "embedding": vector}},
		"usage": map[string]any{"prompt_tokens": 1, "total_tokens": 1},
	})
}

// TestNewClientRefusesAnAPIWithNoEmbeddingClient fails if a provider whose
// API has no embedding client (Anthropic's) gets a client, or the refusal
// does not name the API.
func TestNewClientRefusesAnAPIWithNoEmbeddingClient(t *testing.T) {
	client, err := NewClient(keyProvider(t, config.APIAnthropic, ""), "claude-test")
	if err == nil || client != nil {
		t.Fatalf("NewClient on an anthropic provider = %v, %v; want a refusal", client, err)
	}
	if !strings.Contains(err.Error(), config.APIAnthropic) {
		t.Fatalf("the refusal %q does not name the provider's API", err)
	}
}

// TestAnOpenAIEmbeddingSendsOneRequestPerInput fails if an OpenAI API
// embedding client sends more than one input in a request (a model gateway's
// gemini-embedding-2 takes one input a call, and answers a list with one
// vector of them all), sends a request without the model, the task type as
// the top-level field task_type, float vectors or the provider's credential,
// or returns other than one vector a text, in the texts' order.
func TestAnOpenAIEmbeddingSendsOneRequestPerInput(t *testing.T) {
	var rec recorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := rec.add(r)
		openAIEmbedding(w, []float32{float32(n + 1), 0.5})
	}))
	defer srv.Close()
	client, err := NewClient(gatewayProvider(t, srv.URL+"/v1"), "gemini-embedding-2")
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{"first chunk", "second chunk", "third chunk"}

	vectors, err := client.Embed(context.Background(), texts, TaskTypeDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	requests := rec.all()
	if len(requests) != len(texts) {
		t.Fatalf("%d requests for %d texts, want one each", len(requests), len(texts))
	}
	for i, req := range requests {
		var body map[string]any
		if err := json.Unmarshal(req.body, &body); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		want := map[string]any{"input": texts[i], "model": "gemini-embedding-2", "task_type": "RETRIEVAL_DOCUMENT", "encoding_format": "float"}
		for key, value := range want {
			if body[key] != value {
				t.Errorf("request %d: %s = %#v, want %#v (body %s)", i, key, body[key], value, req.body)
			}
		}
		if _, ok := body["extra_body"]; ok {
			t.Errorf("request %d carries an extra_body field, which a gateway refuses: %s", i, req.body)
		}
		if auth := req.header.Get("Authorization"); req.path != "/v1/embeddings" || auth != "Bearer id-1" {
			t.Errorf("request %d to %q with Authorization %q; want /v1/embeddings with the provider's ID token", i, req.path, auth)
		}
	}
	if len(vectors) != len(texts) {
		t.Fatalf("%d vectors for %d texts, want one each: %v", len(vectors), len(texts), vectors)
	}
	for i, v := range vectors {
		if len(v) != 2 || v[0] != float32(i+1) {
			t.Fatalf("vectors %v, want the answer to text i at index i", vectors)
		}
	}
}

// TestAGeminiEmbeddingSendsTheTaskType fails if a Vertex AI or Gemini API
// embedding client drops the task type, sends more than one text a call,
// returns the vectors out of order, or sends the call without the provider's
// credential.
func TestAGeminiEmbeddingSendsTheTaskType(t *testing.T) {
	for _, tc := range []struct {
		name, header, want, path string
		provider                 func(t *testing.T, url string) *providers.Provider
	}{
		{
			name: "vertex", header: "Authorization", want: "Bearer tok-1",
			path:     "/projects/example-project/locations/global/publishers/google/models/gemini-embedding-2:embedContent",
			provider: vertexProvider,
		},
		{
			name: "gemini", header: "X-Goog-Api-Key", want: "sk-1",
			path:     "/models/gemini-embedding-2:batchEmbedContents",
			provider: func(t *testing.T, url string) *providers.Provider { return keyProvider(t, config.APIGemini, url) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := rec.add(r)
				values := []float32{float32(n + 1)}
				w.Header().Set("Content-Type", "application/json")
				// Vertex AI's embedContent answers "embedding"; the Gemini
				// API's batchEmbedContents answers "embeddings".
				_ = json.NewEncoder(w).Encode(map[string]any{
					"embedding":  map[string]any{"values": values},
					"embeddings": []map[string]any{{"values": values}},
				})
			}))
			defer srv.Close()
			client, err := NewClient(tc.provider(t, srv.URL), "gemini-embedding-2")
			if err != nil {
				t.Fatal(err)
			}
			texts := []string{"first question", "second question"}

			vectors, err := client.Embed(context.Background(), texts, TaskTypeQuery)
			if err != nil {
				t.Fatalf("Embed: %v", err)
			}
			requests := rec.all()
			if len(requests) != len(texts) {
				t.Fatalf("%d requests for %d texts, want one each", len(requests), len(texts))
			}
			for i, req := range requests {
				if !strings.HasSuffix(req.path, tc.path) {
					t.Errorf("request %d to %q, want a path ending %s", i, req.path, tc.path)
				}
				if !strings.Contains(string(req.body), `"taskType":"RETRIEVAL_QUERY"`) || !strings.Contains(string(req.body), texts[i]) {
					t.Errorf("request %d body %s lacks the task type or text %q", i, req.body, texts[i])
				}
				if got := req.header.Get(tc.header); got != tc.want {
					t.Errorf("request %d: %s = %q, want %q", i, tc.header, got, tc.want)
				}
			}
			if len(vectors) != 2 || vectors[0][0] != 1 || vectors[1][0] != 2 {
				t.Fatalf("vectors %v, want the answer to text i at index i", vectors)
			}
		})
	}
}

// TestAGatewayRetryAfterReachesTheRetry fails if an OpenAI API embedding
// client's error for a 429 does not carry the answer's Retry-After to the
// retry (go-openai's errors keep no header), carries one the answer did not
// name, or loses the 429 status the retry reads.
func TestAGatewayRetryAfterReachesTheRetry(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		want         time.Duration
		named        bool
	}{
		{name: "named", header: "7", want: 7 * time.Second, named: true},
		{name: "not named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				rateLimit(w)
			}))
			defer srv.Close()
			client, err := NewClient(gatewayProvider(t, srv.URL+"/v1"), "gemini-embedding-2")
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.Embed(context.Background(), []string{"a chunk"}, TaskTypeDocument)
			var answered *providererr.Error
			if !errors.As(err, &answered) || answered.Status != http.StatusTooManyRequests {
				t.Fatalf("Embed = %v; want the gateway's 429", err)
			}
			var after *retry.AfterError
			if named := errors.As(err, &after); named != tc.named || (named && after.After != tc.want) {
				t.Fatalf("Embed = %#v; want Retry-After named %v, %s", err, tc.named, tc.want)
			}
		})
	}
}

// rateLimit answers a call 429, as a model gateway past its limit does.
func rateLimit(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"error":{"message":"rate limit","type":"rate_limit_error","code":"429"}}`)
}
