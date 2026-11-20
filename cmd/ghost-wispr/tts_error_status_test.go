package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/tts"
)

type ttsTestToken struct{}

func (ttsTestToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "test-token", Expiry: time.Now().Add(time.Hour)}, nil
}

// TestASpeechErrorShowsNoRoomWord: a speech provider that refuses the text
// can quote it in its error. The speak request's status, which
// GET /api/tts/status/{id} returns, and GET /api/logs keep the provider's
// status and code, and no word of its message.
func TestASpeechErrorShowsNoRoomWord(t *testing.T) {
	const planted = "quokkaplantedroomword"
	for _, name := range []string{"gemini", "google", "elevenlabs"} {
		t.Run(name, func(t *testing.T) {
			keepTheDefaultLogger(t)
			sink := logging.NewLogSink(1000)
			logging.SetDefault(logging.New(sink, "debug"))
			quoted := 0
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), planted) {
					quoted++
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Rejected token %s"},"detail":{"status":"invalid_text","message":"Rejected token %s"}}`, planted, planted)
			}))
			t.Cleanup(fake.Close)
			var p tts.Provider
			var err error
			switch name {
			case "gemini":
				p, err = tts.NewGeminiProvider("project", "model", "Kore", tts.WithGeminiAuthCredentials(auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: ttsTestToken{}})), tts.WithGeminiBaseURL(fake.URL))
			case "google":
				p, err = tts.NewGoogleProvider("test-key", "voice", tts.WithGoogleBaseURL(fake.URL))
			case "elevenlabs":
				p, err = tts.NewElevenLabsProvider("test-key", "voice", tts.WithElevenLabsBaseURL(fake.URL))
			}
			if err != nil {
				t.Fatal(err)
			}
			o := tts.NewOrchestrator(p, nil, tts.OrchestratorOpts{})
			t.Cleanup(o.Stop)
			id, err := o.Speak("We agreed on "+planted+" today.", "", "")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			status, err := o.WaitForSynthesis(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if quoted != 1 || status.Status != tts.StatusFailed {
				t.Fatalf("the provider quoted the planted word %d times and the request is %q, want 1 and failed", quoted, status.Status)
			}
			h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), nil, &server.ControlHooks{
				GetLogs: sink.GetLogs,
				TTSStatus: func(id string) (map[string]any, bool) {
					s, ok := o.Status(id)
					return ttsStatusResponse(&s), ok
				},
			}, "", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/api/tts/status/" + id, "/api/logs?limit=1000"} {
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
				if strings.Contains(rr.Body.String(), planted) || strings.Contains(rr.Body.String(), "Rejected") {
					t.Errorf("%s holds the provider's message: %s", path, rr.Body.String())
				}
				if path != "/api/logs?limit=1000" && (rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "HTTP 400")) {
					t.Errorf("%s = %d %s, want the status naming HTTP 400", path, rr.Code, rr.Body.String())
				}
			}
		})
	}
}
