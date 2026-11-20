package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// TestASignInFailureKeepsItsReasonThroughProviderErr fails if a request a
// provider cannot sign in for (its key not set, its ID token not minted, its
// OpenAI exchange refused) fails, once providererr.From reduces it as it does
// the SDKs' errors, as withheld instead of with the sign-in's own reason and
// the provider's name; or if that reason then quotes what the token endpoint
// answered.
func TestASignInFailureKeepsItsReasonThroughProviderErr(t *testing.T) {
	const planted = "quokkaplantedroomword"
	var reached atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_grant","message":"`+planted+`"}}`)
	}))
	defer exchange.Close()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	mint := func(ctx context.Context, file, audience string) (string, error) {
		if audience == "example-gateway" {
			return "", errors.New("default Google credentials name no external-account file")
		}
		return m.IDToken(ctx, file, audience)
	}
	set := New(&config.Config{Providers: map[string]config.Provider{
		"nokey":   {API: config.APIDeepgram, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_UNSET_KEY"}},
		"gateway": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
		"openai":  {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthOpenAIWIF, IdentityProviderID: "idp-x", ServiceAccountID: "sa-x"}},
	}}, Options{IDToken: mint, OpenAITokenEndpoint: exchange.URL, Now: clk.Now})
	for name, want := range map[string]string{
		"nokey":   `provider "nokey" signs in with a key from EXAMPLE_UNSET_KEY: its key is not set`,
		"gateway": `provider "gateway": mint a Google ID token for example-gateway: default Google credentials name no external-account file`,
		"openai":  `provider "openai": openai token: exchange refused: HTTP 401: invalid_grant`,
	} {
		p, _ := set.Get(name)
		resp, err := p.HTTPClient(nil).Post(srv.URL+"/v1/embeddings", "application/json", strings.NewReader(`{"input":"x"}`))
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("%s: a request with no credential answered HTTP %d", name, resp.StatusCode)
		}
		reduced := providererr.From(p.API, err).Error()
		if !strings.Contains(reduced, want) || strings.Contains(reduced, "withheld") || strings.Contains(reduced, planted) {
			t.Errorf("%s: reduced to %q; want the sign-in's reason %q, and nothing the token endpoint said", name, reduced, want)
		}
	}
	if reached.Load() != 0 {
		t.Fatalf("the provider was sent %d requests without a credential", reached.Load())
	}
	nokey, _ := set.Get("nokey")
	if _, err := nokey.Credential(context.Background()); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Credential = %v; errors.Is no longer finds ErrNoKey", err)
	}
}
