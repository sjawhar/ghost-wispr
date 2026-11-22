package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAnEnvironmentBaseURLDoesNotMoveAProvidersCalls fails if genai's
// GOOGLE_VERTEX_BASE_URL or GOOGLE_GEMINI_BASE_URL, set in the unit's
// environment, sends a Vertex AI or Gemini API provider's calls to another
// host than the provider's own.
func TestAnEnvironmentBaseURLDoesNotMoveAProvidersCalls(t *testing.T) {
	t.Setenv("GOOGLE_VERTEX_BASE_URL", "https://elsewhere.example.test/")
	t.Setenv("GOOGLE_GEMINI_BASE_URL", "https://elsewhere.example.test/")
	set := New(&config.Config{
		Providers: map[string]config.Provider{
			"vertex":    {API: config.APIVertex, Project: "example-project", Location: "europe-west4", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
			"vertex-us": {API: config.APIVertex, Project: "example-project", Location: "us", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
			"gemini":    {API: config.APIGemini, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_GEMINI_KEY"}},
		},
		ProviderKeys: map[string]string{"EXAMPLE_GEMINI_KEY": "gm-1"},
	}, Options{GoogleCredentials: staticGoogle})
	for name, want := range map[string]string{
		"vertex":    "europe-west4-aiplatform.googleapis.com",
		"vertex-us": "aiplatform.us.rep.googleapis.com",
		"gemini":    "generativelanguage.googleapis.com",
	} {
		p, _ := set.Get(name)
		cc, err := p.GenAIConfig()
		if err != nil {
			t.Fatal(err)
		}
		var host string
		cc.HTTPClient.Transport.(*authTransport).next = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			host = r.URL.Host
			return &http.Response{
				StatusCode: http.StatusOK, Request: r,
				Header: http.Header{"Content-Type": {"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}`)),
			}, nil
		})
		client, err := genai.NewClient(context.Background(), cc)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Models.GenerateContent(context.Background(), "gemini-example", genai.Text("hi"), nil)
		if err != nil || host != want {
			t.Errorf("%s: a call went to %q (%v); want %s, the provider's own host", name, host, err, want)
		}
	}
}
