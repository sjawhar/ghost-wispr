package embedding

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// TestARefusalNamesTheProviderNotItsAPI fails if a refusal by a provider
// named gateway, which speaks the OpenAI API, is reported as OpenAI's, in
// the error's first word or in its answer: an operator would look for the
// fault at api.openai.com.
func TestARefusalNamesTheProviderNotItsAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"team may not call this model","type":"team_model_access_denied","code":"403"}}`)
	}))
	defer srv.Close()
	mint := func(context.Context, string, string) (string, error) { return "id-1", nil }
	p, err := providers.New(&config.Config{Providers: map[string]config.Provider{
		"gateway": {API: config.APIOpenAI, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
	}}, providers.Options{IDToken: mint}).Get("gateway")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(p, "gemini-embedding-example")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Embed(context.Background(), []string{"hello"}, TaskTypeDocument)
	if err == nil || !strings.HasPrefix(err.Error(), "gateway embedding: gateway answered HTTP 403 (team_model_access_denied)") || strings.Contains(err.Error(), "openai") {
		t.Fatalf("Embed = %v; want an error of the gateway's embedding, naming no openai", err)
	}
}
