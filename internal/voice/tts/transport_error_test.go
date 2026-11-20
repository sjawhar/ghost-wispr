package tts

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

	"cloud.google.com/go/auth"
)

const transportPlanted = "quokkaplantedroomword"

// brokenTransport answers each request with err, or with status and a body
// whose read fails with bodyErr after body.
type brokenTransport struct {
	err, bodyErr error
	status       int
	body         string
}

func (b brokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.err != nil {
		return nil, b.err
	}
	return &http.Response{
		StatusCode: b.status,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(io.MultiReader(strings.NewReader(b.body), errReader{b.bodyErr})),
		Request:    r,
	}, nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// A Gemini stream that fails in transport, or whose stream or refusal cannot
// be read, fails without quoting what Vertex AI sent: net/http's own errors
// quote an answer it could not read, as an HTTP/2 GOAWAY's debug text does.
func TestAGeminiTransportFailureQuotesNothing(t *testing.T) {
	goaway := errors.New(`http2: server sent GOAWAY and closed the connection; debug="` + transportPlanted + `"`)
	for name, transport := range map[string]brokenTransport{
		"transport":    {err: errors.New(`malformed HTTP status code "` + transportPlanted + `400"`)},
		"stream":       {status: http.StatusOK, body: sseAudio(ramp(0, 960)), bodyErr: goaway},
		"refusal body": {status: http.StatusForbidden, bodyErr: goaway},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: "http://vertex.example", HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			err = g.Stream(context.Background(), "hello", func([]byte) error { return nil })
			if err == nil || strings.Contains(err.Error(), transportPlanted) {
				t.Fatalf("Stream = %v, want a failure that quotes nothing Vertex AI sent", err)
			}
		})
	}
}

// refusedToken is a token source Google refuses with an answer that quotes
// the planted word, as an auth error carries it.
type refusedToken struct{}

func (refusedToken) Token(context.Context) (*auth.Token, error) {
	return nil, &auth.Error{
		Response: &http.Response{StatusCode: http.StatusForbidden},
		Body:     []byte(`{"error":"invalid_grant","error_description":"cannot authenticate ` + transportPlanted + `"}`),
	}
}

// A token Google refuses fails the stream with Google's status and OAuth
// code, not its answer, which the auth error prints.
func TestARefusedGoogleTokenQuotesNothing(t *testing.T) {
	g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: refusedToken{}}), BaseURL: "http://vertex.example"})
	if err != nil {
		t.Fatal(err)
	}
	err = g.Stream(context.Background(), "hello", func([]byte) error { return nil })
	if err == nil || strings.Contains(err.Error(), transportPlanted) || !strings.Contains(err.Error(), "HTTP 403 (invalid_grant)") {
		t.Fatalf("Stream = %v, want Google's 403 and its code, and nothing of its answer", err)
	}
}

// An answer to Aura's handshake that net/http cannot read fails the stream
// without quoting it.
func TestAMalformedAuraHandshakeAnswerQuotesNothing(t *testing.T) {
	for _, answer := range []string{
		"HTTP/1.1 " + transportPlanted + "400 Bad\r\n\r\n",
		"HTTP/1.1 101 Switching Protocols\r\n" + transportPlanted + "\r\n\r\n",
	} {
		a, err := NewAura(AuraConfig{APIKey: fakeAuraKey, BaseURL: "ws://" + malformedHTTPServer(t, answer)})
		if err != nil {
			t.Fatal(err)
		}
		err = a.Stream(context.Background(), "hello", func([]byte) error { return nil })
		if err == nil || strings.Contains(err.Error(), transportPlanted) {
			t.Errorf("Stream = %v, want a failure that quotes nothing Deepgram sent, for %q", err, answer)
		}
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
