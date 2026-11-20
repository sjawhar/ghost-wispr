package gptlive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/spokentext"
)

// TokenEndpoint is OpenAI's token exchange for workload identity federation
// (https://developers.openai.com/api/reference/workload-identity-federation).
const TokenEndpoint = "https://auth.openai.com/oauth/token"

// Audience is the audience of the Google ID token OpenAI's identity provider
// accepts: `ghost-wispr google-id-token --audience https://api.openai.com/v1`.
const Audience = "https://api.openai.com/v1"

// tokenRenewBefore is how long before its expiry a cached access token is
// exchanged again, so a session never opens on a token about to lapse. A token
// that lives less than twice as long is renewed at half its life.
const tokenRenewBefore = 5 * time.Minute

// tokenExchangeTimeout bounds one exchange, whatever the caller's deadline:
// the Supervisor's is the exchange's 2 minutes, and a wake must not wait
// that long on a token endpoint that does not answer.
var tokenExchangeTimeout = 15 * time.Second

// tokenExpiryMargin is how long before its expiry a cached token stops
// being handed out when its renewal has failed.
const tokenExpiryMargin = 30 * time.Second

// TokenConfig configures a TokenSource. IdentityProviderID and
// ServiceAccountID are ids, not secrets (config
// `voice.gptlive.openai_identity_provider_id` and
// `voice.gptlive.openai_service_account_id`).
type TokenConfig struct {
	// Endpoint is the token exchange URL; empty means TokenEndpoint.
	Endpoint string
	// Audience is the Google ID token's audience; empty means Audience. The
	// probe's control asks for another, which OpenAI must refuse.
	Audience           string
	IdentityProviderID string
	ServiceAccountID   string
	// Subject mints the Google ID token for audience, as googleauth.IDToken
	// does with the unit's credential file.
	Subject func(ctx context.Context, audience string) (string, error)
	// Client makes the exchange; nil means http.DefaultClient.
	Client *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// TokenSource exchanges the unit's Google ID token for an OpenAI access token
// that lasts an hour at most, keeps it, and exchanges again before it lapses.
// It never logs or reports either token.
type TokenSource struct {
	cfg TokenConfig

	mu       sync.Mutex
	token    string
	renewAt  time.Time
	expireAt time.Time
}

// NewTokenSource makes a TokenSource. It keeps a copy of config.
func NewTokenSource(config *TokenConfig) (*TokenSource, error) {
	cfg := *config
	if strings.TrimSpace(cfg.IdentityProviderID) == "" {
		return nil, errors.New("openai token: no identity provider id")
	}
	if strings.TrimSpace(cfg.ServiceAccountID) == "" {
		return nil, errors.New("openai token: no service account id")
	}
	if cfg.Subject == nil {
		return nil, errors.New("openai token: no subject token source")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = TokenEndpoint
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Audience == "" {
		cfg.Audience = Audience
	}
	return &TokenSource{cfg: cfg}, nil
}

// Token returns an access token that is valid now, exchanging a new one when
// the one it holds is due for renewal. Concurrent callers share one exchange.
// A renewal that fails while the token it holds has more than
// tokenExpiryMargin left hands that token out instead.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	if s.token != "" && now.Before(s.renewAt) {
		return s.token, nil
	}
	token, expireAt, err := s.exchange(ctx, now)
	if err != nil {
		if s.token != "" && now.Before(s.expireAt.Add(-tokenExpiryMargin)) {
			return s.token, nil
		}
		return "", err
	}
	s.token, s.expireAt = token, expireAt
	s.renewAt = expireAt.Add(-min(tokenRenewBefore, expireAt.Sub(now)/2))
	return token, nil
}

// tokenExchangeRequest is the token exchange's request body.
type tokenExchangeRequest struct {
	GrantType          string `json:"grant_type"`
	SubjectTokenType   string `json:"subject_token_type"`
	SubjectToken       string `json:"subject_token"`
	IdentityProviderID string `json:"identity_provider_id"`
	ServiceAccountID   string `json:"service_account_id"`
}

type tokenExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	ExpiresAt   int64  `json:"expires_at"`
}

// tokenExchangeError is the token exchange's error body. OpenAI answers with
// an API error object; an OAuth error string is read too.
type tokenExchangeError struct {
	Error json.RawMessage `json:"error"`
}

func (s *TokenSource) exchange(ctx context.Context, now time.Time) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenExchangeTimeout)
	defer cancel()
	subject, err := s.cfg.Subject(ctx, s.cfg.Audience)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("openai token: mint Google ID token: %w", err)
	}
	body, err := json.Marshal(tokenExchangeRequest{
		GrantType:          "urn:ietf:params:oauth:grant-type:token-exchange",
		SubjectTokenType:   "urn:ietf:params:oauth:token-type:id_token",
		SubjectToken:       subject,
		IdentityProviderID: s.cfg.IdentityProviderID,
		ServiceAccountID:   s.cfg.ServiceAccountID,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("openai token: encode exchange: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("openai token: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.cfg.Client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("openai token: exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("openai token: read exchange answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("openai token: exchange refused: HTTP %d: %s", resp.StatusCode, exchangeErrorCode(data, subject))
	}
	var answer tokenExchangeResponse
	if err := json.Unmarshal(data, &answer); err != nil {
		return "", time.Time{}, fmt.Errorf("openai token: parse exchange answer: %w", err)
	}
	if answer.AccessToken == "" {
		return "", time.Time{}, errors.New("openai token: exchange answer has no access_token")
	}
	if answer.TokenType != "" && !strings.EqualFold(answer.TokenType, "bearer") {
		return "", time.Time{}, fmt.Errorf("openai token: exchange answer has token_type %q, want Bearer", answer.TokenType)
	}
	// expires_at is the token's exp claim, and expires_in counts from its
	// issue; they should agree, and where they do not the earlier holds.
	var expireAt time.Time
	if answer.ExpiresAt > 0 {
		expireAt = time.Unix(answer.ExpiresAt, 0)
	}
	if answer.ExpiresIn > 0 {
		if in := now.Add(time.Duration(answer.ExpiresIn) * time.Second); expireAt.IsZero() || in.Before(expireAt) {
			expireAt = in
		}
	}
	if !expireAt.After(now) {
		return "", time.Time{}, errors.New("openai token: exchange answer has expired")
	}
	return answer.AccessToken, expireAt, nil
}

// exchangeErrorCode names a refused exchange's structured code only. A
// provider's message can echo the Google ID token, the audio it received or
// a room word; Open wraps this error and Supervisor logs it. It is therefore
// never returned. spokentext.Withhold detects a direct echo of the subject
// token before the stricter policy below withholds every free-form message.
func exchangeErrorCode(data []byte, subject string) string {
	var body tokenExchangeError
	if json.Unmarshal(data, &body) != nil || len(body.Error) == 0 {
		return "server_refusal"
	}
	var apiErr struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body.Error, &apiErr) == nil {
		_ = spokentext.Withhold(apiErr.Message, subject)
		if apiErr.Code != "" {
			return apiErr.Code
		}
		if apiErr.Type != "" {
			return apiErr.Type
		}
		return "server_refusal"
	}
	var oauthCode string
	if json.Unmarshal(body.Error, &oauthCode) == nil && oauthCode != "" {
		return oauthCode
	}
	return "server_refusal"
}
