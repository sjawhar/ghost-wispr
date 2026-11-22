package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// fakeJWT is an unsigned JWT that expires at exp.
func fakeJWT(n int, exp time.Time) string {
	claims, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "n": n})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// minter mints numbered ID tokens that last an hour from the clock's now.
type minter struct {
	clock     *clock
	n         atomic.Int32
	audiences []string
	mu        sync.Mutex
}

func (m *minter) IDToken(_ context.Context, _, audience string) (string, error) {
	m.mu.Lock()
	m.audiences = append(m.audiences, audience)
	m.mu.Unlock()
	return fakeJWT(int(m.n.Add(1)), m.clock.Now().Add(time.Hour)), nil
}

func gatewayConfig(url string) *config.Config {
	return &config.Config{Providers: map[string]config.Provider{
		"gateway": {API: config.APIOpenAI, URL: url, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
	}}
}

// TestACredentialGoesInTheHeaderItsAPIReads fails if a key is not sent the way
// its vendor reads one (Deepgram's "Token", Anthropic's x-api-key, Google's
// x-goog-api-key, ElevenLabs' xi-api-key), if a token is not a bearer token, or
// if a placeholder an SDK set stays beside it.
func TestACredentialGoesInTheHeaderItsAPIReads(t *testing.T) {
	for _, c := range []struct {
		api, kind, header, want string
	}{
		{config.APIOpenAI, config.AuthAPIKey, "Authorization", "Bearer cred"},
		{config.APIOpenAI, config.AuthGoogleIDToken, "Authorization", "Bearer cred"},
		{config.APIOpenAI, config.AuthOpenAIWIF, "Authorization", "Bearer cred"},
		{config.APIAnthropic, config.AuthAPIKey, "X-Api-Key", "cred"},
		{config.APIAnthropic, config.AuthGoogleIDToken, "Authorization", "Bearer cred"},
		{config.APIDeepgram, config.AuthAPIKey, "Authorization", "Token cred"},
		{config.APIGemini, config.AuthAPIKey, "X-Goog-Api-Key", "cred"},
		{config.APIGemini, config.AuthGoogleIDToken, "Authorization", "Bearer cred"},
		{config.APIGemini, config.AuthGoogle, "Authorization", "Bearer cred"},
		{config.APIVertex, config.AuthGoogle, "Authorization", "Bearer cred"},
		{config.APIVertex, config.AuthAPIKey, "X-Goog-Api-Key", "cred"},
		{config.APIElevenLabs, config.AuthAPIKey, "Xi-Api-Key", "cred"},
		{config.APIGoogleTTS, config.AuthGoogle, "Authorization", "Bearer cred"},
		{config.APIGoogleTTS, config.AuthAPIKey, "X-Goog-Api-Key", "cred"},
	} {
		p := &Provider{Name: "p", Provider: config.Provider{API: c.api, Auth: config.ProviderAuth{Kind: c.kind}}}
		h := http.Header{"Authorization": {"Bearer placeholder"}, "X-Api-Key": {"placeholder"}, "X-Goog-Api-Key": {"placeholder"}}
		p.SetAuth(h, "cred")
		if got := h.Get(c.header); got != c.want {
			t.Errorf("%s with %s: %s = %q, want %q", c.api, c.kind, c.header, got, c.want)
		}
		for name, values := range h {
			if name != c.header && len(values) > 0 {
				t.Errorf("%s with %s: %s still holds %q", c.api, c.kind, name, values)
			}
		}
	}
}

// TestAnIDTokenIsMintedOnceUntilItIsDue fails if a Google ID token is minted
// for each request, is minted for any audience but the provider's, or is held
// past five minutes before its expiry.
func TestAnIDTokenIsMintedOnceUntilItIsDue(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	set := New(gatewayConfig("https://gateway.example.test/v1"), Options{IDToken: m.IDToken, Now: clk.Now})
	p, err := set.Get("gateway")
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clk.Add(54 * time.Minute)
	if again, _ := p.Credential(context.Background()); again != first || m.n.Load() != 1 {
		t.Fatalf("at 54 minutes: %d mints, same token %v; want the one token, minted once", m.n.Load(), again == first)
	}
	clk.Add(2 * time.Minute)
	if renewed, _ := p.Credential(context.Background()); renewed == first || m.n.Load() != 2 {
		t.Fatalf("at 56 minutes, 4 before expiry: %d mints; want a second token", m.n.Load())
	}
	if m.audiences[0] != "example-gateway" {
		t.Fatalf("minted for %q, want the provider's audience", m.audiences[0])
	}
}

// TestARefusedTokenIsRenewedAndTheRequestSentAgain fails if a request the
// provider refuses with 401 is not sent once more, whole, with a new token, or
// if a key provider's 401 is sent again.
func TestARefusedTokenIsRenewedAndTheRequestSentAgain(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	var seen []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Header.Get("Authorization"))
		bodies = append(bodies, string(body))
		if len(seen) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	set := New(gatewayConfig(srv.URL+"/v1"), Options{IDToken: m.IDToken, Now: clk.Now})
	p, _ := set.Get("gateway")
	resp, err := p.HTTPClient(nil).Post(srv.URL+"/v1/embeddings", "application/json", strings.NewReader(`{"input":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(seen) != 2 || seen[0] == seen[1] || bodies[1] != `{"input":"x"}` {
		t.Fatalf("status %d after %d sends with tokens %q and bodies %q; want a second send, whole, with a new token", resp.StatusCode, len(seen), seen, bodies)
	}

	seen = nil
	keyed := New(&config.Config{
		Providers:    map[string]config.Provider{"k": {API: config.APIOpenAI, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}}},
		ProviderKeys: map[string]string{"EXAMPLE_KEY": "sk-example"},
	}, Options{})
	k, _ := keyed.Get("k")
	resp, err = k.HTTPClient(nil).Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(seen) != 1 || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a key provider's 401 was sent %d times; a key cannot be renewed, so once", len(seen))
	}
}

// TestTheGatewayCheckAsksTheGatewayWithTheIDToken fails if the check of a
// gateway signed in by ID token does not list its models with that token, so
// a gateway that refuses the unit reads as working; if a refusal (401, 403)
// reads as signed in, or another failing status as a refusal; or if it
// reports the token or the answer.
func TestTheGatewayCheckAsksTheGatewayWithTheIDToken(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	var answer atomic.Int32 // the status the gateway answers; 0 lists its models
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if status := int(answer.Load()); status != 0 {
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"error":"unknown caller %s"}`, gotAuth)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	set := New(gatewayConfig(srv.URL+"/v1"), Options{IDToken: m.IDToken, Now: clk.Now})
	p, _ := set.Get("gateway")
	if s := p.Check(context.Background()); !s.OK || !s.Asked || !strings.HasPrefix(gotAuth, "Bearer eyJ") || s.Auth != config.AuthGoogleIDToken || !strings.Contains(s.SignIn, "example-gateway") {
		t.Fatalf("check %+v with Authorization %q; want OK and asked, by the ID token", s, gotAuth)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	for status, want := range map[int]string{
		http.StatusUnauthorized:        host + " refused the credential (HTTP 401).",
		http.StatusForbidden:           host + " refused the credential (HTTP 403).",
		http.StatusInternalServerError: "Signed in, but " + host + " answered HTTP 500.",
	} {
		answer.Store(int32(status))
		s := p.Check(context.Background())
		if s.OK || !s.Asked || s.Message != want {
			t.Errorf("a gateway that answers %d: %+v, want not OK, asked, saying %q", status, s, want)
		}
		token := strings.TrimPrefix(gotAuth, "Bearer ")
		if strings.Contains(s.Message, token) || strings.Contains(s.Message, "unknown caller") {
			t.Fatalf("the check's message quotes the answer or the token: %q", s.Message)
		}
	}
}

// TestAKeyProviderWithNoKeyFailsItsCheck fails if a provider whose variable is
// empty reads as working, or its status does not name the variable.
func TestAKeyProviderWithNoKeyFailsItsCheck(t *testing.T) {
	set := New(&config.Config{Providers: map[string]config.Provider{
		"deepgram": {API: config.APIDeepgram, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_DEEPGRAM_KEY"}},
	}}, Options{})
	p, _ := set.Get("deepgram")
	s := p.Check(context.Background())
	if s.OK || s.KeySet || s.KeyEnv != "EXAMPLE_DEEPGRAM_KEY" || !strings.Contains(s.Message, "not set") {
		t.Fatalf("check = %+v; want not OK, the variable named and the key not set", s)
	}
}

// TestOpenAIWorkloadIdentityChecksByExchangingAToken fails if an openai_wif
// provider's check does not exchange the Google ID token at OpenAI's token
// endpoint, with the provider's two ids, or reads a refused exchange as OK.
func TestOpenAIWorkloadIdentityChecksByExchangingAToken(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	var refuse atomic.Bool
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if refuse.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_grant","message":"no"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"oa-1","token_type":"Bearer","expires_in":3600}`)
	}))
	defer srv.Close()
	cfg := &config.Config{Providers: map[string]config.Provider{
		"openai": {API: config.APIOpenAI, Auth: config.ProviderAuth{Kind: config.AuthOpenAIWIF, IdentityProviderID: "idp-example", ServiceAccountID: "sa-example"}},
	}}
	p, _ := New(cfg, Options{IDToken: m.IDToken, OpenAITokenEndpoint: srv.URL, Now: clk.Now}).Get("openai")
	if s := p.Check(context.Background()); !s.OK || s.Asked || got["identity_provider_id"] != "idp-example" || got["service_account_id"] != "sa-example" {
		t.Fatalf("check %+v after an exchange of %v; want OK, not asked (the exchange is the check), with the provider's ids", s, got)
	}
	refuse.Store(true)
	p, _ = New(cfg, Options{IDToken: m.IDToken, OpenAITokenEndpoint: srv.URL, Now: clk.Now}).Get("openai")
	if s := p.Check(context.Background()); s.OK || !strings.Contains(s.Message, "invalid_grant") {
		t.Fatalf("a refused exchange: %+v, want not OK, naming its code", s)
	}
}

// TestStatusesAreCheckedOnceAWhile fails if a status younger than maxAge is
// checked again, or an older one is not.
func TestStatusesAreCheckedOnceAWhile(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	set := New(&config.Config{
		Providers:    map[string]config.Provider{"k": {API: config.APIOpenAI, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}}},
		ProviderKeys: map[string]string{"EXAMPLE_KEY": "sk-example"},
	}, Options{Now: clk.Now})
	for range 3 {
		if s := set.Statuses(context.Background(), 5*time.Minute); len(s) != 1 || !s[0].OK {
			t.Fatalf("statuses %+v", s)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("%d checks in five minutes, want 1", reads.Load())
	}
	clk.Add(6 * time.Minute)
	set.Statuses(context.Background(), 5*time.Minute)
	if reads.Load() != 2 {
		t.Fatalf("%d checks after the status aged, want 2", reads.Load())
	}
}

// TestAVertexProviderInAMultiRegionUsesItsHost fails if a Vertex AI provider
// in the us multi-region is sent to a regional host genai would make up.
func TestAVertexProviderInAMultiRegionUsesItsHost(t *testing.T) {
	set := New(&config.Config{Providers: map[string]config.Provider{
		"vertex-us": {API: config.APIVertex, Project: "example-project", Location: "us", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
		"vertex":    {API: config.APIVertex, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
	}}, Options{})
	us, _ := set.Get("vertex-us")
	cc, err := us.GenAIConfig()
	if err != nil || cc.HTTPOptions.BaseURL != "https://aiplatform.us.rep.googleapis.com/" || cc.Project != "example-project" || cc.Location != "us" {
		t.Fatalf("us config = %+v, %v; want the us multi-region's host", cc, err)
	}
	global, _ := set.Get("vertex")
	if cc, _ := global.GenAIConfig(); cc.HTTPOptions.BaseURL != "https://aiplatform.googleapis.com/" {
		t.Fatalf("global config's base URL = %q, want the global host, which no environment variable can move", cc.HTTPOptions.BaseURL)
	}
}

// TestAProviderIsReadyOnlyWithEveryPartOfItsSignIn fails if a provider with
// no key, or OpenAI workload identity with an id missing, reads as ready, so
// a voice would be built that fails every call, or if a whole one does not.
func TestAProviderIsReadyOnlyWithEveryPartOfItsSignIn(t *testing.T) {
	set := New(&config.Config{
		Providers: map[string]config.Provider{
			"keyed":   {API: config.APIDeepgram, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}},
			"nokey":   {API: config.APIDeepgram, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_OTHER_KEY"}},
			"wif":     {API: config.APIOpenAI, Auth: config.ProviderAuth{Kind: config.AuthOpenAIWIF, IdentityProviderID: "idp-x", ServiceAccountID: "sa-x"}},
			"halfwif": {API: config.APIOpenAI, Auth: config.ProviderAuth{Kind: config.AuthOpenAIWIF, IdentityProviderID: "idp-x"}},
			"vertex":  {API: config.APIVertex, Project: "p", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
		},
		ProviderKeys: map[string]string{"EXAMPLE_KEY": "k-1"},
	}, Options{})
	for name, ready := range map[string]bool{"keyed": true, "nokey": false, "wif": true, "halfwif": false, "vertex": true} {
		p, _ := set.Get(name)
		if err := p.Ready(); (err == nil) != ready {
			t.Errorf("%s: Ready() = %v, want ready %v", name, err, ready)
		}
	}
}
