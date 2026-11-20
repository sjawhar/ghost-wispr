package transcribe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const transportPlanted = "quokkaplantedroomword"

// brokenTransport answers each request with err, or with a 200 whose body
// read fails with bodyErr.
type brokenTransport struct{ err, bodyErr error }

func (b brokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.err != nil {
		return nil, b.err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(errReader{b.bodyErr}), Request: r}, nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// A Deepgram batch call that fails in transport, or whose answer cannot be
// read, fails without quoting what Deepgram sent: net/http's own errors
// quote an answer it could not read, as an HTTP/2 GOAWAY's debug text does.
func TestADeepgramTransportFailureQuotesNothing(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "session.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, transport := range map[string]brokenTransport{
		"transport": {err: errors.New(`malformed HTTP status code "` + transportPlanted + `400"`)},
		"body":      {bodyErr: errors.New(`http2: server sent GOAWAY and closed the connection; debug="` + transportPlanted + `"`)},
	} {
		t.Run(name, func(t *testing.T) {
			transcriber := NewDeepgramBatchTranscriber(DeepgramBatchConfig{BaseURL: "https://deepgram.example", HTTPClient: &http.Client{Transport: transport}})
			_, err := transcriber.Transcribe(context.Background(), audioPath)
			if err == nil || strings.Contains(err.Error(), transportPlanted) {
				t.Fatalf("Transcribe = %v, want a failure that quotes nothing Deepgram sent", err)
			}
		})
	}
}
