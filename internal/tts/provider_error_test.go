package tts

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth"
)

// plantedWord stands for one room word in the text to speak: a single
// lower-case word, which a filter that looks for a quote of the text cannot
// tell from an ordinary word.
const plantedWord = "quokkaplantedroomword"

// staticToken is a token source that always answers.
type staticToken struct{}

func (staticToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "test-token", Expiry: time.Now().Add(time.Hour)}, nil
}

// refusedToken is a token source Google refuses, with an answer that quotes
// the planted word, as an auth error carries it.
type refusedToken struct{}

func (refusedToken) Token(context.Context) (*auth.Token, error) {
	return nil, &auth.Error{
		Response: &http.Response{StatusCode: http.StatusForbidden},
		Body:     []byte(`{"error":"invalid_grant","error_description":"cannot authenticate ` + plantedWord + `"}`),
	}
}

// providersAt returns each TTS provider, pointed at baseURL, with client as
// its HTTP client when set.
func providersAt(t *testing.T, baseURL string, client *http.Client, token auth.TokenProvider) map[string]Provider {
	t.Helper()
	elevenLabs, err := NewElevenLabsProvider("test-key", "voice", WithElevenLabsBaseURL(baseURL))
	if err != nil {
		t.Fatal(err)
	}
	google, err := NewGoogleProvider("test-key", "voice", WithGoogleBaseURL(baseURL))
	if err != nil {
		t.Fatal(err)
	}
	gemini, err := NewGeminiProvider("project", "model", "Kore", WithGeminiAuthCredentials(auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: token})), WithGeminiBaseURL(baseURL))
	if err != nil {
		t.Fatal(err)
	}
	if client != nil {
		elevenLabs.httpClient, google.httpClient, gemini.httpClient = client, client, client
	}
	return map[string]Provider{"elevenlabs": elevenLabs, "google": google, "gemini": gemini}
}

// TestATTSErrorKeepsOnlyItsStatusAndCode: a speech provider can quote in its
// error the text it was asked to speak, and the error is kept in the speak
// request's status (GET /api/tts/status/{id}). The error keeps the
// provider, its status and its error code, and none of the provider's
// message, even one that quotes a single room word.
func TestATTSErrorKeepsOnlyItsStatusAndCode(t *testing.T) {
	quoted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), plantedWord) {
			quoted++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Rejected token %s"},"detail":{"status":"invalid_text","message":"Rejected token %s"}}`, plantedWord, plantedWord)
	}))
	t.Cleanup(server.Close)
	wantCode := map[string]string{"elevenlabs": "invalid_text", "google": "INVALID_ARGUMENT", "gemini": "INVALID_ARGUMENT"}
	for name, p := range providersAt(t, server.URL, nil, staticToken{}) {
		_, err := p.Synthesize(context.Background(), SpeechRequest{Text: "We agreed on " + plantedWord + " today."})
		if err == nil {
			t.Fatalf("%s: no error from an answer of status 400", name)
		}
		if strings.Contains(err.Error(), plantedWord) || strings.Contains(err.Error(), "Rejected") {
			t.Errorf("%s: the error quotes the provider's message: %v", name, err)
		}
		if !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), wantCode[name]) {
			t.Errorf("%s: the error %q does not name status 400 and code %s", name, err, wantCode[name])
		}
	}
	if quoted != 3 {
		t.Fatalf("the providers sent the planted word %d times, want 3, so the check read nothing", quoted)
	}
}

// A speech request that fails before an answer comes, or whose answer
// cannot be read, fails without quoting what the provider sent: a refused
// token quotes Google's answer, net/http quotes an answer it could not read,
// as a malformed status line or an HTTP/2 GOAWAY's debug text.
func TestATTSTransportFailureQuotesNothing(t *testing.T) {
	text := "We agreed on " + plantedWord + " today."
	t.Run("malformed answer", func(t *testing.T) {
		address := malformedAnswerServer(t, "HTTP/1.1 "+plantedWord+"400 Bad\r\n\r\n")
		for name, p := range providersAt(t, "http://"+address, nil, staticToken{}) {
			if _, err := p.Synthesize(context.Background(), SpeechRequest{Text: text}); err == nil || strings.Contains(err.Error(), plantedWord) {
				t.Errorf("%s: Synthesize = %v, want a failure that quotes nothing the server sent", name, err)
			}
		}
	})
	t.Run("unreadable body", func(t *testing.T) {
		goaway := errors.New(`http2: server sent GOAWAY and closed the connection; debug="` + plantedWord + `"`)
		for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
			client := &http.Client{Transport: brokenBody{status: status, err: goaway}}
			for name, p := range providersAt(t, "http://tts.example", client, staticToken{}) {
				if _, err := p.Synthesize(context.Background(), SpeechRequest{Text: text}); err == nil || strings.Contains(err.Error(), plantedWord) {
					t.Errorf("%s, status %d: Synthesize = %v, want the body's read failure withheld", name, status, err)
				}
			}
		}
	})
	t.Run("refused token", func(t *testing.T) {
		p := providersAt(t, "http://tts.example", nil, refusedToken{})["gemini"]
		_, err := p.Synthesize(context.Background(), SpeechRequest{Text: text})
		if err == nil || strings.Contains(err.Error(), plantedWord) || !strings.Contains(err.Error(), "HTTP 403 (invalid_grant)") {
			t.Errorf("Synthesize = %v, want Google's 403 and its code, and nothing of its answer", err)
		}
	})
}

// brokenBody answers each request with status and a body whose read fails
// with err.
type brokenBody struct {
	status int
	err    error
}

func (b brokenBody) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: b.status, Header: http.Header{}, Body: io.NopCloser(failingReader{b.err}), Request: r}, nil
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// malformedAnswerServer answers every connection's request with answer, then
// closes it, and returns its address.
func malformedAnswerServer(t *testing.T, answer string) string {
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
