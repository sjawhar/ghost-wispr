package openaiwif

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const transportPlanted = "quokkaplantedroomword"

// brokenTransport answers each request with err, or with status and a body
// whose read fails with bodyErr.
type brokenTransport struct {
	err, bodyErr error
	status       int
	body         string
}

func (b brokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.err != nil {
		return nil, b.err
	}
	body := io.NopCloser(strings.NewReader(b.body))
	if b.bodyErr != nil {
		body = io.NopCloser(io.MultiReader(strings.NewReader(b.body), errReader{b.bodyErr}))
	}
	return &http.Response{StatusCode: b.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body, Request: r}, nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// A token exchange that fails in transport, whose answer cannot be read, or
// whose answer names a token type other than Bearer, fails without quoting
// what OpenAI sent: net/http's own errors quote an answer it could not read,
// as an HTTP/2 GOAWAY's debug text does.
func TestATokenExchangeFailureQuotesNothingOpenAISent(t *testing.T) {
	for name, transport := range map[string]brokenTransport{
		"transport":  {err: errors.New(`malformed HTTP status code "` + transportPlanted + `400"`)},
		"body":       {status: http.StatusOK, bodyErr: errors.New(`http2: server sent GOAWAY and closed the connection; debug="` + transportPlanted + `"`)},
		"token type": {status: http.StatusOK, body: `{"access_token":"a1","token_type":"` + transportPlanted + `","expires_in":3600}`},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := NewTokenSource(&TokenConfig{
				Endpoint:           "https://auth.example/oauth/token",
				IdentityProviderID: "idp_test",
				ServiceAccountID:   "user-test",
				Subject:            func(context.Context, string) (string, error) { return "google-id-token", nil },
				Client:             &http.Client{Transport: transport},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Token(context.Background())
			if err == nil || strings.Contains(err.Error(), transportPlanted) {
				t.Fatalf("Token = %v, want a failure that quotes nothing OpenAI sent", err)
			}
		})
	}
}

// A refused exchange's error, which a voice's Open wraps and the Supervisor
// logs, never holds the subject token, even when OpenAI's message echoes it,
// and keeps the refusal's code.
func TestARefusedExchangeNeverQuotesTheSubjectToken(t *testing.T) {
	for _, echo := range []bool{false, true} {
		name := "ordinary refusal control"
		if echo {
			name = "refusal echoes subject"
		}
		t.Run(name, func(t *testing.T) {
			clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
			e := newTokenEndpoint(t, clock.Now)
			e.status = http.StatusUnauthorized
			message := "Invalid subject token"
			const secret = "sol-review-synthetic-subject-not-a-real-token"
			if echo {
				message += ": " + secret
			}
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "invalid_subject_token", "message": message}})
			e.body = string(body)
			tokens := newTestTokenSource(t, e, clock, func(context.Context, string) (string, error) { return secret, nil })
			_, err := tokens.Token(context.Background())
			if err == nil {
				t.Fatal("expected refusal")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("the subject token reaches the exchange's error")
			}
			if !strings.Contains(err.Error(), "invalid_subject_token") {
				t.Fatal("lost useful error code")
			}
		})
	}
}

// A code or type that is free text, not a plain token, is left out of a
// refusal's code, as a message is.
func TestARefusalsCodeIsAPlainToken(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"bad content someone said","type":"the words someone said"}}`,
		`{"error":"the words someone said"}`,
	} {
		if got := ErrorCode([]byte(body), ""); got != "server_refusal" {
			t.Errorf("ErrorCode(%s) = %q, want server_refusal", body, got)
		}
	}
	if got := ErrorCode([]byte(`{"error":{"code":"invalid_api_key","type":"the words"}}`), ""); got != "invalid_api_key" {
		t.Errorf("ErrorCode kept %q, want invalid_api_key", got)
	}
}
