package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// authRecorder keeps every credential header a fake provider is sent.
type authRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (a *authRecorder) record(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range authHeaders {
		if v := r.Header.Get(name); v != "" {
			a.seen = append(a.seen, name+": "+v)
		}
	}
}

func (a *authRecorder) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

// TestACredentialNeverFollowsARedirectToAnotherOrigin fails if a provider's
// client, or its check, sends the provider's credential to another origin
// the provider redirects it to, or reads that redirect as a success. The
// three providers carry their credential in Authorization as a key, in
// X-Goog-Api-Key as a key, and in Authorization as a minted token.
func TestACredentialNeverFollowsARedirectToAnotherOrigin(t *testing.T) {
	var other authRecorder
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.record(r)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer a.Close()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	set := New(&config.Config{
		Providers: map[string]config.Provider{
			"deepgram": {API: config.APIDeepgram, URL: a.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_DEEPGRAM_KEY"}},
			"gemini":   {API: config.APIGemini, URL: a.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_GEMINI_KEY"}},
			"gateway":  {API: config.APIOpenAI, URL: a.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
		},
		ProviderKeys: map[string]string{"EXAMPLE_DEEPGRAM_KEY": "dg-1", "EXAMPLE_GEMINI_KEY": "gm-1"},
	}, Options{IDToken: m.IDToken, Now: clk.Now})
	for _, name := range []string{"deepgram", "gemini", "gateway"} {
		p, _ := set.Get(name)
		resp, err := p.HTTPClient(nil).Get(a.URL + "/v1/models")
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: a redirect to another origin answered HTTP %d, want the call refused", name, resp.StatusCode)
		} else if !strings.Contains(err.Error(), "redirected") || !strings.Contains(err.Error(), a.URL) {
			t.Errorf("%s: error %q; want it to say the provider at %s redirected the call away", name, err, a.URL)
		} else if reduced := providererr.From(p.API, err).Error(); !strings.Contains(reduced, "redirected") || strings.Contains(reduced, b.URL) {
			// The SDKs' errors reach the log through providererr.From.
			t.Errorf("%s: reduced to %q; want the reason kept and the redirect's target dropped", name, reduced)
		}
		if s := p.Check(context.Background()); s.OK || s.Asked || !strings.Contains(s.Message, "redirected") {
			t.Errorf("%s: check %+v; want not OK, not asked, saying the provider redirected it away", name, s)
		}
	}
	if seen := other.all(); len(seen) > 0 {
		t.Fatalf("the other origin was sent %q", seen)
	}
}

// TestASameOriginRedirectKeepsTheCredential fails if a provider's client, or
// its check, stops at a redirect within the provider's own origin, or sends
// the next hop without the credential.
func TestASameOriginRedirectKeepsTheCredential(t *testing.T) {
	var seen authRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		seen.record(r)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	p, _ := New(&config.Config{
		Providers:    map[string]config.Provider{"deepgram": {API: config.APIDeepgram, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_DEEPGRAM_KEY"}}},
		ProviderKeys: map[string]string{"EXAMPLE_DEEPGRAM_KEY": "dg-1"},
	}, Options{}).Get("deepgram")
	resp, err := p.HTTPClient(nil).Get(srv.URL + "/v1/listen")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(seen.all()) != 1 || seen.all()[0] != "Authorization: Token dg-1" {
		t.Fatalf("status %d, the redirect's target was sent %q; want 200 with the key", resp.StatusCode, seen.all())
	}
	if s := p.Check(context.Background()); !s.OK || !s.Asked || len(seen.all()) != 2 || seen.all()[1] != "Authorization: Token dg-1" {
		t.Fatalf("check %+v, the redirect's target was sent %q; want OK, asked, with the key", s, seen.all())
	}
}

// TestAnHTTPSProviderNeverSendsItsCredentialOverHTTP fails if a provider
// whose url is https sends its credential in a plain http request to its own
// host and port.
func TestAnHTTPSProviderNeverSendsItsCredentialOverHTTP(t *testing.T) {
	var seen authRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	p, _ := New(&config.Config{
		Providers:    map[string]config.Provider{"deepgram": {API: config.APIDeepgram, URL: "https://" + strings.TrimPrefix(srv.URL, "http://"), Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_DEEPGRAM_KEY"}}},
		ProviderKeys: map[string]string{"EXAMPLE_DEEPGRAM_KEY": "dg-1"},
	}, Options{}).Get("deepgram")
	resp, err := p.HTTPClient(nil).Get(srv.URL + "/v1/listen")
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || len(seen.all()) > 0 {
		t.Fatalf("a plain http request to an https provider's host: error %v, the host was sent %q; want it refused, sent nothing", err, seen.all())
	}
}
