// Package providers signs Ghost Wispr in to the model providers its config
// names (config.Provider): it holds each one's credential (a key, a Google
// access or ID token, or an OpenAI access token given in exchange for a
// Google ID token), renews it, puts it in the header the provider's API
// reads, sends it nowhere but the provider's own origin, and checks that it
// works.
package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/openaiwif"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// The public base URL of each API, as its own SDK takes it: the OpenAI API's
// ends in /v1. A provider's url replaces it.
var defaultURLs = map[string]string{
	config.APIOpenAI:     "https://api.openai.com/v1",
	config.APIAnthropic:  "https://api.anthropic.com",
	config.APIGemini:     "https://generativelanguage.googleapis.com",
	config.APIDeepgram:   "https://api.deepgram.com",
	config.APIElevenLabs: "https://api.elevenlabs.io",
	config.APIGoogleTTS:  "https://texttospeech.googleapis.com",
}

// VertexHost is the Vertex AI host of a location: the global host, a
// multi-region's own host ("us", "eu"), or a region's.
func VertexHost(location string) string {
	switch location {
	case "", "global":
		return "https://aiplatform.googleapis.com"
	case "us", "eu":
		return "https://aiplatform." + location + ".rep.googleapis.com"
	default:
		return "https://" + location + "-aiplatform.googleapis.com"
	}
}

// Options are what the Set signs in with. The zero value uses the unit's
// own: googleauth for Google, OpenAI's token exchange and net/http.
type Options struct {
	// IDToken mints a Google ID token for audience from the credential file;
	// googleauth.IDToken when nil.
	IDToken func(ctx context.Context, credentialsFile, audience string) (string, error)
	// GoogleCredentials detects Google credentials from the credential file,
	// or Application Default Credentials without one; googleauth.Detect,
	// once a credential file for the process, when nil.
	GoogleCredentials func(ctx context.Context, credentialsFile string, scopes ...string) (*auth.Credentials, error)
	// OpenAITokenEndpoint replaces OpenAI's token exchange, for tests.
	OpenAITokenEndpoint string
	// Client makes the token exchanges and the checks; http.DefaultClient
	// when nil. A provider's own requests go through Provider.HTTPClient.
	Client *http.Client
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Set is the providers of one config, each with its sign-in. Build a new
// Set when the providers change.
type Set struct {
	providers map[string]*Provider
	opts      Options

	mu     sync.Mutex
	status map[string]Status // the last check of each provider
	// keyGen counts each provider's key saves (SetKeys): a check is cached
	// only while the provider's count is the one it began at, so a check of
	// a key replaced meanwhile never replaces the saved key's status.
	keyGen map[string]uint64
}

// New builds the Set of cfg's providers. It contacts no provider.
func New(cfg *config.Config, opts Options) *Set {
	if opts.IDToken == nil {
		opts.IDToken = googleauth.IDToken
	}
	if opts.GoogleCredentials == nil {
		opts.GoogleCredentials = detectOnce
	}
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Set{providers: make(map[string]*Provider, len(cfg.Providers)), opts: opts, status: map[string]Status{}, keyGen: map[string]uint64{}}
	uses := cfg.ProviderModelUses()
	for name := range cfg.Providers {
		s.providers[name] = &Provider{
			Name:            name,
			Provider:        cfg.Providers[name],
			key:             cfg.APIKey(name),
			credentialsFile: cfg.GoogleCredentialsFile,
			uses:            uses[name],
			set:             s,
		}
	}
	return s
}

// Get returns the provider named name.
func (s *Set) Get(name string) (*Provider, error) {
	p, ok := s.providers[name]
	if !ok {
		return nil, fmt.Errorf("no provider %q", name)
	}
	return p, nil
}

// SetKeys gives each provider that signs in with a key the key cfg holds
// for it now, so a client built before a key was saved sends the new one,
// and drops the last check of each provider whose key changed; a check of
// the key it replaced still under way is not cached when it ends. A settings
// save changes keys and nothing else of a provider; the Set is built again
// for any other change.
func (s *Set) SetKeys(cfg *config.Config) {
	for name, p := range s.providers {
		if p.Auth.Kind != config.AuthAPIKey {
			continue
		}
		key := cfg.APIKey(name)
		if p.apiKey() == key {
			continue
		}
		p.keyMu.Lock()
		p.key = key
		p.keyMu.Unlock()
		s.mu.Lock()
		s.keyGen[name]++
		delete(s.status, name)
		s.mu.Unlock()
	}
}

// apiKey is the key a provider that signs in with one holds now.
func (p *Provider) apiKey() string {
	p.keyMu.RLock()
	defer p.keyMu.RUnlock()
	return p.key
}

// Model resolves a role's provider/model against cfg and returns the
// provider, signed in, and the model as the provider names it.
func (s *Set) Model(cfg *config.Config, role config.Role, ref string) (*Provider, string, error) {
	m, err := cfg.ResolveModel(role, ref)
	if err != nil {
		return nil, "", err
	}
	p, err := s.Get(m.Provider)
	return p, m.Model, err
}

// Provider is one configured provider with its credential.
type Provider struct {
	Name string
	config.Provider

	keyMu           sync.RWMutex
	key             string // an api_key provider's key, which SetKeys replaces
	credentialsFile string
	uses            []config.ModelUse // the models the roles in use call on it, which its check asks about
	set             *Set

	mu       sync.Mutex
	google   *auth.Credentials      // a google provider's credentials
	idToken  cachedToken            // a google_id_token provider's token
	exchange *openaiwif.TokenSource // an openai_wif provider's exchange
}

// cachedToken is a token held until shortly before its expiry.
type cachedToken struct {
	value   string
	renewAt time.Time
}

// ErrNoKey is why an api_key provider cannot sign in: its variable is empty.
var ErrNoKey = errors.New("its key is not set")

// BaseURL is the provider's url, or its API's public base URL; for Vertex
// AI, the host of its location. It ends in no slash.
func (p *Provider) BaseURL() string {
	if u := strings.TrimRight(strings.TrimSpace(p.URL), "/"); u != "" {
		return u
	}
	if p.API == config.APIVertex {
		return VertexHost(p.Location)
	}
	return defaultURLs[p.API]
}

// SocketURL is BaseURL as a WebSocket URL: wss:// for https://, ws:// for
// http://. A url of another scheme is kept as written.
func (p *Provider) SocketURL() string {
	base := p.BaseURL()
	if rest, ok := strings.CutPrefix(base, "https://"); ok {
		return "wss://" + rest
	}
	if rest, ok := strings.CutPrefix(base, "http://"); ok {
		return "ws://" + rest
	}
	return base
}

// Ready says whether the provider can try to sign in as configured, asking
// no one: a key is set, an ID token names its audience, OpenAI workload
// identity names both its ids. A voice or the Concierge built on a provider
// that is not ready would fail every call.
func (p *Provider) Ready() error {
	switch p.Auth.Kind {
	case config.AuthAPIKey:
		if p.apiKey() == "" {
			return fmt.Errorf("provider %q signs in with a key from %s: %w", p.Name, p.Auth.Env, ErrNoKey)
		}
	case config.AuthGoogleIDToken:
		if strings.TrimSpace(p.Auth.Audience) == "" {
			return fmt.Errorf("provider %q signs in with a Google ID token and names no auth.audience", p.Name)
		}
	case config.AuthOpenAIWIF:
		if strings.TrimSpace(p.Auth.IdentityProviderID) == "" || strings.TrimSpace(p.Auth.ServiceAccountID) == "" {
			return fmt.Errorf("provider %q signs in with OpenAI workload identity: set auth.identity_provider_id and auth.service_account_id", p.Name)
		}
	case config.AuthGoogle:
	default:
		return fmt.Errorf("provider %q signs in with %q, which this build does not know", p.Name, p.Auth.Kind)
	}
	return nil
}

// Credential returns what the provider's requests carry now: its key, or a
// token, minted or exchanged when the one held is due for renewal. A
// provider that is not Ready has none. Its error names the provider and why
// it could not sign in, and holds nothing a provider answered (this
// package's, googleauth's and openaiwif's errors withhold that), so it is a
// providererr.OwnFailure: providererr.From keeps its text.
func (p *Provider) Credential(ctx context.Context) (string, error) {
	credential, err := p.credential(ctx)
	if err != nil {
		return "", &signInError{err: err}
	}
	return credential, nil
}

// signInError is why a provider could not sign in.
type signInError struct{ err error }

func (e *signInError) Error() string           { return e.err.Error() }
func (e *signInError) Unwrap() error           { return e.err }
func (e *signInError) ProviderFailure() string { return e.err.Error() }

func (p *Provider) credential(ctx context.Context) (string, error) {
	if err := p.Ready(); err != nil {
		return "", err
	}
	switch p.Auth.Kind {
	case config.AuthAPIKey:
		return p.apiKey(), nil
	case config.AuthGoogle:
		creds, err := p.GoogleCredentials(ctx)
		if err != nil {
			return "", err
		}
		token, err := creds.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("provider %q: Google access token: %w", p.Name, providererr.From("google", err))
		}
		return token.Value, nil
	case config.AuthGoogleIDToken:
		return p.googleIDToken(ctx)
	default: // config.AuthOpenAIWIF: Ready admits no other kind
		exchange, err := p.openAIExchange()
		if err != nil {
			return "", err
		}
		token, err := exchange.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("provider %q: %w", p.Name, err)
		}
		return token, nil
	}
}

// Renew drops the token the provider holds, so the next Credential mints or
// exchanges a new one: a provider answered 401 to the one it held. A google
// provider's credentials hold their token until it is due, so Renew drops
// them, and the process's detection of their file, to be detected again.
func (p *Provider) Renew() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.idToken = cachedToken{}
	p.exchange = nil // a new exchange holds no token
	if p.Auth.Kind == config.AuthGoogle {
		p.google = nil
		googleCredentialsCache.Delete(p.credentialsFile)
	}
}

// GoogleCredentials returns a google provider's credentials, detected once.
func (p *Provider) GoogleCredentials(ctx context.Context) (*auth.Credentials, error) {
	if p.Auth.Kind != config.AuthGoogle {
		return nil, fmt.Errorf("provider %q signs in with %s, not Google credentials", p.Name, p.Auth.Kind)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.google == nil {
		creds, err := p.set.opts.GoogleCredentials(ctx, p.credentialsFile, googleauth.CloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("provider %q: detect Google credentials: %w", p.Name, err)
		}
		p.google = creds
	}
	return p.google, nil
}

// idTokenRenewBefore is how long before its expiry an ID token is minted
// again. One whose expiry cannot be read is held for idTokenHold.
const (
	idTokenRenewBefore = 5 * time.Minute
	idTokenHold        = 50 * time.Minute
)

func (p *Provider) googleIDToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.set.opts.Now()
	if p.idToken.value != "" && now.Before(p.idToken.renewAt) {
		return p.idToken.value, nil
	}
	token, err := p.set.opts.IDToken(ctx, p.credentialsFile, p.Auth.Audience)
	if err != nil {
		return "", fmt.Errorf("provider %q: mint a Google ID token for %s: %w", p.Name, p.Auth.Audience, err)
	}
	renewAt := now.Add(idTokenHold)
	if exp, ok := jwtExpiry(token); ok {
		renewAt = exp.Add(-idTokenRenewBefore)
	}
	p.idToken = cachedToken{value: token, renewAt: renewAt}
	return token, nil
}

func (p *Provider) openAIExchange() (*openaiwif.TokenSource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exchange != nil {
		return p.exchange, nil
	}
	file, mint := p.credentialsFile, p.set.opts.IDToken
	exchange, err := openaiwif.NewTokenSource(&openaiwif.TokenConfig{
		Endpoint:           p.set.opts.OpenAITokenEndpoint,
		IdentityProviderID: p.Auth.IdentityProviderID,
		ServiceAccountID:   p.Auth.ServiceAccountID,
		Subject: func(ctx context.Context, audience string) (string, error) {
			return mint(ctx, file, audience)
		},
		Client: p.set.opts.Client,
		Now:    p.set.opts.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", p.Name, err)
	}
	p.exchange = exchange
	return exchange, nil
}

// authHeaders are the headers any API reads a credential from; a request a
// provider's SDK built may carry a placeholder in one, which SetAuth drops.
var authHeaders = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Xi-Api-Key"}

// SetAuth puts credential in the header the provider's API reads it from:
// a key as each vendor takes one, a token as a bearer token.
func (p *Provider) SetAuth(h http.Header, credential string) {
	for _, name := range authHeaders {
		h.Del(name)
	}
	key := p.Auth.Kind == config.AuthAPIKey
	switch {
	case key && p.API == config.APIAnthropic:
		h.Set("X-Api-Key", credential)
	case key && (p.API == config.APIGemini || p.API == config.APIVertex || p.API == config.APIGoogleTTS):
		h.Set("X-Goog-Api-Key", credential)
	case key && p.API == config.APIElevenLabs:
		h.Set("Xi-Api-Key", credential)
	case key && p.API == config.APIDeepgram:
		h.Set("Authorization", "Token "+credential)
	default:
		h.Set("Authorization", "Bearer "+credential)
	}
}

// Header returns the auth header of a request made now, for a socket dial.
func (p *Provider) Header(ctx context.Context) (http.Header, error) {
	credential, err := p.Credential(ctx)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	p.SetAuth(h, credential)
	return h, nil
}

// renews says whether a 401 can be answered with a new credential.
func (p *Provider) renews() bool {
	return p.Auth.Kind == config.AuthGoogle || p.Auth.Kind == config.AuthGoogleIDToken || p.Auth.Kind == config.AuthOpenAIWIF
}

// origin is a URL's scheme and host, in lower case, with the scheme's
// default port written out: what a credential is bound to.
func origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "ws": "80", "https": "443", "wss": "443"}[scheme]
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// ownOrigin says whether u is at the provider's origin, its BaseURL's scheme
// and host: the only place its credential goes. An https provider's
// credential never goes over http, nor to another host or port.
func (p *Provider) ownOrigin(u *url.URL) bool {
	base, err := url.Parse(p.BaseURL())
	return err == nil && base.Host != "" && origin(base) == origin(u)
}

// originError is a request, or a redirect, to another origin than the
// provider's, refused so that its credential goes nowhere else. It names the
// provider's origin alone: a redirect's target is the provider's own words.
type originError struct {
	provider, origin string
	redirect         bool
}

func (e *originError) Error() string {
	if e.redirect {
		return fmt.Sprintf("provider %q at %s redirected the call to another origin, where its credential never goes, so the call stopped", e.provider, e.origin)
	}
	return fmt.Sprintf("provider %q sends its credential only to %s, so a request to another origin was refused", e.provider, e.origin)
}

// ProviderFailure makes the error a providererr.OwnFailure: its text, which
// names only the configured origin, survives providererr.From.
func (e *originError) ProviderFailure() string { return e.Error() }

func (p *Provider) originError(redirect bool) error {
	shown := p.BaseURL()
	if base, err := url.Parse(shown); err == nil && base.Host != "" {
		shown = base.Scheme + "://" + base.Host
	}
	return &originError{provider: p.Name, origin: shown, redirect: redirect}
}

// checkRedirect is a client's redirect policy next (net/http's own when
// nil) behind a refusal of any redirect away from the provider's origin.
func (p *Provider) checkRedirect(next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !p.ownOrigin(req.URL) {
			return p.originError(true)
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
}

// HTTPClient returns a client whose every request carries the provider's
// credential in its API's header, and that sends a request refused with 401
// once more with a renewed token. It refuses a request, and stops at a
// redirect, to another origin than the provider's. base is the client it
// wraps; nil wraps http.DefaultTransport with no timeout.
func (p *Provider) HTTPClient(base *http.Client) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.Transport = &authTransport{p: p, next: next}
	c.CheckRedirect = p.checkRedirect(c.CheckRedirect)
	return c
}

type authTransport struct {
	p    *Provider
	next http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.p.ownOrigin(req.URL) {
		if req.Body != nil {
			_ = req.Body.Close() // a RoundTripper closes the body, even when it refuses
		}
		return nil, t.p.originError(false)
	}
	send := func(r *http.Request) (*http.Response, error) {
		credential, err := t.p.Credential(r.Context())
		if err != nil {
			return nil, err
		}
		r = r.Clone(r.Context())
		t.p.SetAuth(r.Header, credential)
		return t.next.RoundTrip(r)
	}
	resp, err := send(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !t.p.renews() {
		return resp, err
	}
	t.p.Renew() // the next request needs a new token even if this body cannot replay
	if req.Body != nil && req.GetBody == nil {
		return resp, nil // the body is spent and cannot be sent again
	}
	retry := req
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return resp, nil
		}
		retry = req.Clone(req.Context())
		retry.Body = body
	}
	_ = resp.Body.Close()
	return send(retry)
}

// googleCredentialsCache holds the Google credentials of each credential
// file, so a rebuilt Set does not detect them again: detection reads files,
// and the credentials cache their tokens.
var googleCredentialsCache sync.Map // credential file → *auth.Credentials

// detectOnce is googleauth.Detect, once a credential file for the process.
func detectOnce(ctx context.Context, file string, scopes ...string) (*auth.Credentials, error) {
	if cached, ok := googleCredentialsCache.Load(file); ok {
		return cached.(*auth.Credentials), nil
	}
	creds, err := googleauth.Detect(ctx, file, scopes...)
	if err != nil {
		return nil, err
	}
	cached, _ := googleCredentialsCache.LoadOrStore(file, creds)
	return cached.(*auth.Credentials), nil
}

// jwtExpiry reads the exp claim of a JWT, unverified: it only says when to
// mint the token again.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
