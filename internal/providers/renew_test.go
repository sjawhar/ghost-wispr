package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// refuser is a fake provider that refuses one credential with 401 and takes
// any other, keeping each request's Authorization and body.
type refuser struct {
	refused string
	mu      sync.Mutex
	auths   []string
	bodies  []string
}

func (f *refuser) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()
	if r.Header.Get("Authorization") == f.refused {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	_, _ = io.WriteString(w, "ok")
}

// sent returns each request's Authorization and body, in order.
func (f *refuser) sent() (auths, bodies []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...), append([]string(nil), f.bodies...)
}

// post sends one JSON request through p's client and returns its status.
func post(t *testing.T, p *Provider, url string) int {
	t.Helper()
	resp, err := p.HTTPClient(nil).Post(url, "application/json", strings.NewReader(`{"input":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestARefusedOpenAITokenIsExchangedAgain fails if a request a provider
// refuses with 401 under the access token OpenAI's workload identity
// exchanged is not sent once more, whole, with the token of a second
// exchange.
func TestARefusedOpenAITokenIsExchangedAgain(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	var exchanges atomic.Int32
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"access_token":"oa-%d","token_type":"Bearer","expires_in":3600}`, exchanges.Add(1))
	}))
	defer tokens.Close()
	api := &refuser{refused: "Bearer oa-1"}
	srv := httptest.NewServer(api)
	defer srv.Close()
	p, _ := New(&config.Config{Providers: map[string]config.Provider{
		"openai": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthOpenAIWIF, IdentityProviderID: "idp-x", ServiceAccountID: "sa-x"}},
	}}, Options{IDToken: m.IDToken, OpenAITokenEndpoint: tokens.URL, Now: clk.Now}).Get("openai")
	status := post(t, p, srv.URL+"/v1/audio/speech")
	auths, bodies := api.sent()
	if status != http.StatusOK || exchanges.Load() != 2 || len(auths) != 2 || auths[1] != "Bearer oa-2" || bodies[1] != `{"input":"x"}` {
		t.Fatalf("status %d after %d exchanges, sent with %q and bodies %q; want a second exchange and the whole request again with its token", status, exchanges.Load(), auths, bodies)
	}
}

// googleTokens issues gt-1, gt-2, …, each good for an hour.
type googleTokens struct{ n *atomic.Int32 }

func (g googleTokens) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: fmt.Sprintf("gt-%d", g.n.Add(1)), Type: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
}

// TestARefusedGoogleTokenIsRenewed fails if a request a provider refuses with
// 401 under a Google access token is not sent once more, whole, with a token
// of credentials detected again: detected credentials hold their token until
// it is due, so only a new detection gets another.
func TestARefusedGoogleTokenIsRenewed(t *testing.T) {
	var detections, minted atomic.Int32
	detect := func(context.Context, string, ...string) (*auth.Credentials, error) {
		detections.Add(1)
		return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: auth.NewCachedTokenProvider(googleTokens{&minted}, nil)}), nil
	}
	api := &refuser{refused: "Bearer gt-1"}
	srv := httptest.NewServer(api)
	defer srv.Close()
	p, _ := New(&config.Config{Providers: map[string]config.Provider{
		"vertex": {API: config.APIVertex, URL: srv.URL, Project: "example-project", Location: "us", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
	}}, Options{GoogleCredentials: detect}).Get("vertex")
	status := post(t, p, srv.URL+"/v1/projects/example-project/locations/us/publishers/google/models/gemini-example:generateContent")
	auths, bodies := api.sent()
	if status != http.StatusOK || detections.Load() != 2 || len(auths) != 2 || auths[1] != "Bearer gt-2" || bodies[1] != `{"input":"x"}` {
		t.Fatalf("status %d after %d detections, sent with %q and bodies %q; want the credentials detected again and the whole request sent again with the new token", status, detections.Load(), auths, bodies)
	}
}

// TestRenewForgetsTheDetectedGoogleCredentials fails if a google provider's
// renewal keeps the process's detection of its credential file, which holds
// the refused token: a Set built after it would sign in with that token.
func TestRenewForgetsTheDetectedGoogleCredentials(t *testing.T) {
	file := filepath.Join(t.TempDir(), "google-credentials.json")
	googleCredentialsCache.Store(file, auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: googleTokens{new(atomic.Int32)}}))
	t.Cleanup(func() { googleCredentialsCache.Delete(file) })
	p, _ := New(&config.Config{
		GoogleCredentialsFile: file,
		Providers:             map[string]config.Provider{"vertex": {API: config.APIVertex, Project: "example-project", Auth: config.ProviderAuth{Kind: config.AuthGoogle}}},
	}, Options{}).Get("vertex")
	p.Renew()
	if _, ok := googleCredentialsCache.Load(file); ok {
		t.Fatal("Renew kept the process's detection of the provider's credential file")
	}
}
