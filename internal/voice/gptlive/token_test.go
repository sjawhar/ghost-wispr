package gptlive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tokenEndpoint is a stand-in for OpenAI's token exchange: it checks each
// request and answers with the next access token, valid for lifetime.
type tokenEndpoint struct {
	t        *testing.T
	server   *httptest.Server
	calls    atomic.Int32
	lifetime time.Duration
	now      func() time.Time
	status   int
	body     string
	requests chan tokenRequest
	// delay holds each answer back, so concurrent callers overlap.
	delay time.Duration
	// hang holds each answer until the request is abandoned.
	hang bool
}

type tokenRequest struct {
	GrantType          string `json:"grant_type"`
	SubjectTokenType   string `json:"subject_token_type"`
	SubjectToken       string `json:"subject_token"`
	IdentityProviderID string `json:"identity_provider_id"`
	ServiceAccountID   string `json:"service_account_id"`
}

func newTokenEndpoint(t *testing.T, now func() time.Time) *tokenEndpoint {
	t.Helper()
	e := &tokenEndpoint{t: t, lifetime: time.Hour, now: now, requests: make(chan tokenRequest, 16)}
	e.server = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.server.Close)
	return e
}

func (e *tokenEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	n := e.calls.Add(1)
	if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
		http.Error(w, "wrong request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		return
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		http.Error(w, "content type "+got, http.StatusUnsupportedMediaType)
		return
	}
	var req tokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case e.requests <- req:
	default:
	}
	if e.hang {
		<-r.Context().Done()
		return
	}
	time.Sleep(e.delay)
	if e.status != 0 {
		w.WriteHeader(e.status)
		_, _ = w.Write([]byte(e.body))
		return
	}
	issued := e.now()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":      "access-" + string(rune('0'+n)),
		"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"token_type":        "Bearer",
		"expires_in":        int(e.lifetime / time.Second),
		"expires_at":        issued.Add(e.lifetime).Unix(),
	})
}

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestTokenSource(t *testing.T, e *tokenEndpoint, clock *fakeClock, subject func(context.Context, string) (string, error)) *TokenSource {
	t.Helper()
	if subject == nil {
		subject = func(context.Context, string) (string, error) { return "google-id-token", nil }
	}
	s, err := NewTokenSource(&TokenConfig{
		Endpoint:           e.server.URL + "/oauth/token",
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "user-test",
		Subject:            subject,
		Client:             e.server.Client(),
		Now:                clock.Now,
	})
	if err != nil {
		t.Fatalf("NewTokenSource: %v", err)
	}
	return s
}

func TestTokenSourceExchangesAGoogleIDTokenForAnOpenAIAccessToken(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	var audience string
	s := newTestTokenSource(t, e, clock, func(_ context.Context, aud string) (string, error) {
		audience = aud
		return "google-id-token", nil
	})

	token, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "access-1" {
		t.Fatalf("token = %q, want the exchanged access token", token)
	}
	if audience != "https://api.openai.com/v1" {
		t.Fatalf("Google ID token audience = %q, want OpenAI's", audience)
	}
	req := <-e.requests
	want := tokenRequest{
		GrantType:          "urn:ietf:params:oauth:grant-type:token-exchange",
		SubjectTokenType:   "urn:ietf:params:oauth:token-type:id_token",
		SubjectToken:       "google-id-token",
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "user-test",
	}
	if req != want {
		t.Fatalf("exchange request = %+v, want %+v", req, want)
	}
}

func TestTokenSourceReusesTheTokenUntilShortlyBeforeItExpires(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	s := newTestTokenSource(t, e, clock, nil)

	first, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	clock.Advance(54 * time.Minute)
	again, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if again != first || e.calls.Load() != 1 {
		t.Fatalf("after 54 of 60 minutes: token %q (first %q), %d exchanges; want the cached token and 1 exchange", again, first, e.calls.Load())
	}

	clock.Advance(2 * time.Minute)
	renewed, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if renewed == first || e.calls.Load() != 2 {
		t.Fatalf("after 56 of 60 minutes: token %q (first %q), %d exchanges; want a new token from a second exchange", renewed, first, e.calls.Load())
	}
}

func TestTokenSourceRenewsAShortLivedTokenAtHalfItsLifetime(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	e.lifetime = 4 * time.Minute
	s := newTestTokenSource(t, e, clock, nil)

	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	clock.Advance(time.Minute)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if e.calls.Load() != 1 {
		t.Fatalf("1 minute into a 4-minute token: %d exchanges, want 1", e.calls.Load())
	}
	clock.Advance(90 * time.Second)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if e.calls.Load() != 2 {
		t.Fatalf("2.5 minutes into a 4-minute token: %d exchanges, want 2", e.calls.Load())
	}
}

func TestTokenSourceNeverUsesAnExpiredToken(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	s := newTestTokenSource(t, e, clock, nil)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}

	clock.Advance(2 * time.Hour)
	e.status, e.body = http.StatusServiceUnavailable, `{"error":{"message":"try later","type":"server_error","code":null}}`
	if token, err := s.Token(context.Background()); err == nil {
		t.Fatalf("Token after expiry with the endpoint down = %q, nil; want an error, not the expired token", token)
	}
}

func TestTokenSourceMakesOneExchangeForConcurrentCallers(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	e.delay = 50 * time.Millisecond // every caller arrives while the first exchange is under way
	s := newTestTokenSource(t, e, clock, nil)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := s.Token(context.Background()); err != nil {
				t.Errorf("Token: %v", err)
			}
		})
	}
	wg.Wait()
	if e.calls.Load() != 1 {
		t.Fatalf("8 concurrent callers made %d exchanges, want 1", e.calls.Load())
	}
}

func TestTokenSourceReportsAFailedExchangeWithoutEitherToken(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	e.status = http.StatusUnauthorized
	e.body = `{"error":{"message":"The provided service_account_id mapping does not match token attributes.","type":"invalid_request_error","param":null,"code":"invalid_grant"}}`
	s := newTestTokenSource(t, e, clock, func(context.Context, string) (string, error) { return "secret-google-id-token", nil })

	_, err := s.Token(context.Background())
	if err == nil {
		t.Fatal("Token with a refused exchange: nil error")
	}
	msg := err.Error()
	for _, want := range []string{"401", "invalid_grant", "mapping does not match"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
	if strings.Contains(msg, "secret-google-id-token") {
		t.Errorf("error %q holds the Google ID token", msg)
	}
}

func TestTokenSourceRefusesAnAnswerWithoutAnAccessToken(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	e.status = http.StatusOK
	e.body = `{"token_type":"Bearer","expires_in":3600}`
	s := newTestTokenSource(t, e, clock, nil)
	if token, err := s.Token(context.Background()); err == nil {
		t.Fatalf("Token = %q, nil; want an error for an answer with no access_token", token)
	}
}

func TestNewTokenSourceRequiresTheProviderAndServiceAccount(t *testing.T) {
	subject := func(context.Context, string) (string, error) { return "t", nil }
	for name, cfg := range map[string]TokenConfig{
		"no provider":        {ServiceAccountID: "user-x", Subject: subject},
		"no service account": {IdentityProviderID: "idp_x", Subject: subject},
		"no subject token":   {IdentityProviderID: "idp_x", ServiceAccountID: "user-x"},
	} {
		if _, err := NewTokenSource(&cfg); err == nil {
			t.Errorf("%s: NewTokenSource accepted %+v", name, cfg)
		}
	}
}

// A renewal that fails while the token it holds is still good keeps the
// unit talking: it uses that token until shortly before it expires.
func TestTokenSourceKeepsAValidTokenWhenARenewalFails(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	s := newTestTokenSource(t, e, clock, nil)
	first, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	clock.Advance(56 * time.Minute)
	e.status, e.body = http.StatusServiceUnavailable, `{"error":{"message":"try later","type":"server_error","code":"unavailable"}}`
	if got, err := s.Token(context.Background()); err != nil || got != first {
		t.Fatalf("Token with a failed renewal 4 minutes before expiry = %q, %v; want the cached token", got, err)
	}
	clock.Advance(3*time.Minute + 40*time.Second)
	if got, err := s.Token(context.Background()); err == nil {
		t.Fatalf("Token 20 s before expiry with the endpoint down = %q, nil; want an error", got)
	}
}

// The Supervisor's context has a 2-minute deadline: a token endpoint that
// never answers must not hold a wake that long.
func TestTokenSourceBoundsAnExchangeThatHangs(t *testing.T) {
	defer func(d time.Duration) { tokenExchangeTimeout = d }(tokenExchangeTimeout)
	tokenExchangeTimeout = 200 * time.Millisecond
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	e.hang = true
	s := newTestTokenSource(t, e, clock, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	begin := time.Now()
	if _, err := s.Token(ctx); err == nil {
		t.Fatal("Token against an endpoint that never answers succeeded")
	}
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("Token took %v against an endpoint that never answers, want the exchange's own limit", took)
	}
}

func TestTokenSourceRenewsByExpiresInAlone(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	e.status, e.body = http.StatusOK, `{"access_token":"a1","token_type":"Bearer","expires_in":3600}`
	s := newTestTokenSource(t, e, clock, nil)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	clock.Advance(56 * time.Minute)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if e.calls.Load() != 2 {
		t.Fatalf("56 minutes into a token that expires_in 3600: %d exchanges, want 2", e.calls.Load())
	}
}

// expires_in and expires_at should agree; where they do not, the earlier one
// holds.
func TestTokenSourceRenewsByTheEarlierOfItsTwoExpiries(t *testing.T) {
	for name, body := range map[string]string{
		"expires_at earlier": `{"access_token":"a1","token_type":"Bearer","expires_in":3600,"expires_at":1800000600}`,
		"expires_in earlier": `{"access_token":"a1","token_type":"Bearer","expires_in":600,"expires_at":1800003600}`,
	} {
		t.Run(name, func(t *testing.T) {
			clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
			e := newTokenEndpoint(t, clock.Now)
			e.status, e.body = http.StatusOK, body
			s := newTestTokenSource(t, e, clock, nil)
			if _, err := s.Token(context.Background()); err != nil {
				t.Fatalf("Token: %v", err)
			}
			clock.Advance(6 * time.Minute)
			if _, err := s.Token(context.Background()); err != nil {
				t.Fatalf("Token: %v", err)
			}
			if e.calls.Load() != 2 {
				t.Fatalf("6 minutes into a 10-minute token: %d exchanges, want 2", e.calls.Load())
			}
		})
	}
}

func TestTokenSourceRefusesAnAnswerItCannotUse(t *testing.T) {
	for name, body := range map[string]string{
		"not a bearer":    `{"access_token":"a1","token_type":"mac","expires_in":3600}`,
		"already expired": `{"access_token":"a1","token_type":"Bearer","expires_at":1799999000}`,
		"no lifetime":     `{"access_token":"a1","token_type":"Bearer"}`,
	} {
		t.Run(name, func(t *testing.T) {
			clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
			e := newTokenEndpoint(t, clock.Now)
			e.status, e.body = http.StatusOK, body
			s := newTestTokenSource(t, e, clock, nil)
			if got, err := s.Token(context.Background()); err == nil {
				t.Fatalf("Token = %q, nil; want an error for %s", got, name)
			}
		})
	}
}

func TestTokenSourceReadsAnOAuthErrorString(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	e.status, e.body = http.StatusBadRequest, `{"error":"invalid_request","error_description":"subject_token is missing"}`
	s := newTestTokenSource(t, e, clock, nil)
	_, err := s.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid_request") || !strings.Contains(err.Error(), "subject_token is missing") {
		t.Fatalf("Token = %v, want the OAuth error and its description", err)
	}
}

func TestTokenSourceReportsAGoogleTokenItCouldNotMint(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	s := newTestTokenSource(t, e, clock, func(context.Context, string) (string, error) {
		return "", errors.New("impersonation denied")
	})
	if _, err := s.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "impersonation denied") {
		t.Fatalf("Token = %v, want the Google ID token's failure", err)
	}
	if e.calls.Load() != 0 {
		t.Fatalf("%d exchanges without a Google ID token", e.calls.Load())
	}
}

// The probe's wrong-audience control mints a Google ID token for another
// audience through the same source.
func TestTokenSourceMintsForTheAudienceItIsGiven(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	e := newTokenEndpoint(t, clock.Now)
	var audience string
	s, err := NewTokenSource(&TokenConfig{
		Endpoint:           e.server.URL + "/oauth/token",
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "user-test",
		Audience:           "https://example.invalid/control",
		Subject: func(_ context.Context, aud string) (string, error) {
			audience = aud
			return "t", nil
		},
		Client: e.server.Client(),
		Now:    clock.Now,
	})
	if err != nil {
		t.Fatalf("NewTokenSource: %v", err)
	}
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if audience != "https://example.invalid/control" {
		t.Fatalf("audience %q, want the configured one", audience)
	}
}
