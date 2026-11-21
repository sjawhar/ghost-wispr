package genaiconfig

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"google.golang.org/genai"
)

func TestResolveBackend(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		project string
		want    string
	}{
		{name: "default to gemini without project", want: BackendGemini},
		{name: "default to vertex with project", project: "proj-123", want: BackendVertex},
		{name: "explicit gemini", backend: BackendGemini, project: "proj-123", want: BackendGemini},
		{name: "explicit vertex with project", backend: BackendVertex, project: "proj-123", want: BackendVertex},
		{name: "vertex falls back without project", backend: BackendVertex, want: BackendGemini},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveBackend(tc.backend, tc.project); got != tc.want {
				t.Fatalf("ResolveBackend(%q, %q) = %q, want %q", tc.backend, tc.project, got, tc.want)
			}
		})
	}
}

func TestBuildClientConfigVertex(t *testing.T) {
	cfg, err := BuildClientConfig(&Options{Backend: BackendVertex, Project: "proj-123"})
	if err != nil {
		t.Fatalf("BuildClientConfig returned error: %v", err)
	}
	if cfg.Backend != genai.BackendVertexAI {
		t.Fatalf("expected Vertex backend, got %v", cfg.Backend)
	}
	if cfg.Project != "proj-123" {
		t.Fatalf("expected project proj-123, got %q", cfg.Project)
	}
	if cfg.Location != DefaultLocation {
		t.Fatalf("expected default location %q, got %q", DefaultLocation, cfg.Location)
	}
	if cfg.APIKey != "" {
		t.Fatalf("expected empty API key for Vertex AI, got %q", cfg.APIKey)
	}
}

func TestBuildClientConfigGemini(t *testing.T) {
	cfg, err := BuildClientConfig(&Options{Backend: BackendGemini, APIKey: "test-key", BaseURL: "http://example.test"})
	if err != nil {
		t.Fatalf("BuildClientConfig returned error: %v", err)
	}
	if cfg.Backend != genai.BackendGeminiAPI {
		t.Fatalf("expected Gemini backend, got %v", cfg.Backend)
	}
	if cfg.APIKey != "test-key" {
		t.Fatalf("expected API key to be preserved, got %q", cfg.APIKey)
	}
	if cfg.HTTPOptions.BaseURL != "http://example.test" {
		t.Fatalf("expected base URL override, got %q", cfg.HTTPOptions.BaseURL)
	}
}

func TestBuildClientConfigFallsBackToGeminiWithoutProject(t *testing.T) {
	cfg, err := BuildClientConfig(&Options{Backend: BackendVertex, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("BuildClientConfig returned error: %v", err)
	}
	if cfg.Backend != genai.BackendGeminiAPI {
		t.Fatalf("expected Gemini fallback backend, got %v", cfg.Backend)
	}
}

func TestBuildClientConfigErrorsWithoutCredentials(t *testing.T) {
	_, err := BuildClientConfig(&Options{Backend: BackendGemini})
	if err == nil {
		t.Fatal("expected error when Gemini backend has no API key")
	}
}

// rotatingToken gives a new ID token on each call: tok-1, tok-2, and so on.
type rotatingToken struct{ n atomic.Int32 }

func (r *rotatingToken) Token(context.Context) (*auth.Token, error) {
	n := r.n.Add(1)
	return &auth.Token{Value: "tok-" + string(rune('0'+n)), Type: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
}

// TestBuildClientConfigGateway fails unless a client on the model gateway
// (Options.GatewayURL) asks Google's own path on the gateway, with the
// credentials' current token in x-goog-api-key on each request, the header
// the gateway reads as the caller's key, and no Authorization header.
func TestBuildClientConfigGateway(t *testing.T) {
	var mu sync.Mutex
	var paths, keys, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		keys = append(keys, r.Header.Get("x-goog-api-key"))
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()
	tokens := &rotatingToken{}
	opts := &Options{
		Backend:            BackendVertex,
		Project:            "ignored",
		GatewayURL:         srv.URL + "/",
		GatewayCredentials: auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: tokens}),
	}
	if !CanUse(opts) {
		t.Fatal("CanUse = false for a gateway with credentials")
	}
	cfg, err := BuildClientConfig(opts)
	if err != nil {
		t.Fatalf("BuildClientConfig: %v", err)
	}
	if cfg.Backend != genai.BackendGeminiAPI {
		t.Fatalf("Backend = %v, want the Gemini API's shape, which the gateway serves", cfg.Backend)
	}
	client, err := genai.NewClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	for range 2 {
		if _, err := client.Models.GenerateContent(context.Background(), "gemini-3.5-flash-lite", genai.Text("hi"), nil); err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("%d requests reached the gateway, want 2", len(paths))
	}
	for i, p := range paths {
		if !strings.HasSuffix(p, "/models/gemini-3.5-flash-lite:generateContent") || !strings.HasPrefix(p, "/v1beta/") {
			t.Errorf("request %d path = %q, want /v1beta/models/gemini-3.5-flash-lite:generateContent", i, p)
		}
		if want := "tok-" + string(rune('1'+i)); keys[i] != want {
			t.Errorf("request %d x-goog-api-key = %q, want the credentials' token %q", i, keys[i], want)
		}
		if auths[i] != "" {
			t.Errorf("request %d carries Authorization %q, want none", i, auths[i])
		}
	}
}

func TestBuildClientConfigGatewayNeedsCredentials(t *testing.T) {
	opts := &Options{GatewayURL: "https://model-gateway.example.test"}
	if CanUse(opts) {
		t.Error("CanUse = true for a gateway with no credentials")
	}
	if _, err := BuildClientConfig(opts); err == nil {
		t.Error("BuildClientConfig made a gateway client with no credentials")
	}
}

// TestTheGatewaysTokenGoesToTheGatewayAlone fails if a redirect from the
// gateway carries the ID token, in x-goog-api-key or Authorization, to
// another host: a different hostname, or the gateway's hostname at another
// port.
func TestTheGatewaysTokenGoesToTheGatewayAlone(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, fmt.Sprintf("%s: goog key %q, auth %q", r.Host, r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization")))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer other.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(other.URL, "http://"), ":")
	for _, target := range []string{"http://localhost:" + port, other.URL} {
		t.Run(target, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			}))
			defer gateway.Close()
			cfg, err := BuildClientConfig(&Options{
				GatewayURL:         gateway.URL,
				GatewayCredentials: auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: &rotatingToken{}}),
			})
			if err != nil {
				t.Fatalf("BuildClientConfig: %v", err)
			}
			client, err := genai.NewClient(context.Background(), cfg)
			if err != nil {
				t.Fatalf("genai.NewClient: %v", err)
			}
			_, _ = client.Models.GenerateContent(context.Background(), "gemini-3.5-flash-lite", genai.Text("hi"), nil)
			mu.Lock()
			defer mu.Unlock()
			for _, seen := range leaked {
				if strings.Contains(seen, "tok-") {
					t.Errorf("the redirect's host saw the gateway's token: %s", seen)
				}
			}
			leaked = nil
		})
	}
}
