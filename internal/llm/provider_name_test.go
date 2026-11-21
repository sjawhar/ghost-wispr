package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// TestARefusalNamesTheProviderNotItsAPI fails if a refusal is reported as
// its API's (openai, anthropic, gemini) rather than as the provider's that
// answered it (a gateway, a Vertex AI location), in the error's first word
// or in its answer: an operator would look for the fault at the vendor.
func TestARefusalNamesTheProviderNotItsAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"no","type":"team_model_access_denied"}}`)
	}))
	defer srv.Close()
	mint := func(context.Context, string, string) (string, error) { return "id-1", nil }
	set := providers.New(&config.Config{
		Providers: map[string]config.Provider{
			"gateway":   {API: config.APIOpenAI, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
			"claude":    {API: config.APIAnthropic, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
			"vertex-us": {API: config.APIVertex, URL: srv.URL, Project: "example-project", Location: "us", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
		},
	}, providers.Options{IDToken: mint, GoogleCredentials: func(context.Context, string, ...string) (*auth.Credentials, error) {
		return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken("tok-1")}), nil
	}})
	for name, api := range map[string]string{"gateway": "openai", "claude": "anthropic", "vertex-us": "gemini"} {
		p, err := set.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(p, "model-example")
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Complete(context.Background(), []Message{{Role: "user", Content: "hello"}})
		if err == nil || !strings.HasPrefix(err.Error(), name+" completion: "+name+" answered HTTP 403") || strings.Contains(err.Error(), api) {
			t.Errorf("%s: Complete = %v; want an error of %s's completion, naming no %s", name, err, name, api)
		}
	}
}
