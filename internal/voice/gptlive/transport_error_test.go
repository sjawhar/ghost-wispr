package gptlive

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
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

// An answer to GPT-Live's socket handshake that net/http cannot read fails
// Open without quoting it.
func TestAMalformedHandshakeAnswerQuotesNothing(t *testing.T) {
	address := malformedHTTPServer(t, "HTTP/1.1 "+transportPlanted+"400 Bad\r\n\r\n")
	v, err := New(Config{URL: "ws://" + address + "/v1/live", Token: func(context.Context) (string, error) { return "access-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := v.Open(context.Background(), testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))); err == nil || strings.Contains(err.Error(), transportPlanted) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("Open = %v, want a failure that quotes nothing the server sent", err)
	}
}

// malformedHTTPServer answers every connection's request with answer, then
// closes it, and returns its address.
func malformedHTTPServer(t *testing.T, answer string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				_, _ = io.WriteString(conn, answer)
			}()
		}
	}()
	return listener.Addr().String()
}
