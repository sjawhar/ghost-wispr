package tts

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// spokenText is a line Wispr is asked to say: a Concierge answer that may
// quote the room.
const spokenText = "purple elephant on the roof of the office"

// TestGeminiWithholdsAMessageThatQuotesTheText fails if Vertex AI's message
// that quotes the text to speak, on a refused request or inside the stream,
// reaches the error, which the Supervisor writes to the journal.
func TestGeminiWithholdsAMessageThatQuotesTheText(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"refused request": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"message":"Sentence starts with \"purple ELEPHANT\" is too long","status":"INVALID_ARGUMENT"}}`)
		},
		"raw body": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "bad request for: "+spokenText)
		},
		"stream error": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"error":{"code":400,"message":"cannot speak: purple elephant","status":"INVALID_ARGUMENT"}}`+"\r\n\r\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)
			g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: srv.URL})
			if err != nil {
				t.Fatalf("NewGemini: %v", err)
			}
			err = g.Stream(context.Background(), spokenText, func([]byte) error { return nil })
			if err == nil {
				t.Fatal("Stream succeeded on a refusal")
			}
			if msg := strings.ToLower(err.Error()); strings.Contains(msg, "purple") || strings.Contains(msg, "elephant") {
				t.Fatalf("the error quotes the text to speak: %v", err)
			}
		})
	}
}

// TestAuraWithholdsAWarningThatQuotesTheText fails if Deepgram's warning that
// quotes the text to speak reaches the Say's error.
func TestAuraWithholdsAWarningThatQuotesTheText(t *testing.T) {
	stall := auraStallTimeout
	auraStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { auraStallTimeout = stall })
	f := startSpeak(t, func(c *speakConn, typ, _ string) {
		if typ == "Flush" {
			_ = c.send(`{"type":"Warning","description":"Text purple elephant on the roof was cut","code":"TEXT_CUT"}`)
		}
	})
	s := openSession(t, newTestAura(t, f), newFakeOutput())
	said := make(chan error, 1)
	go func() { said <- s.Say(context.Background(), spokenText, voice.Fact) }()
	select {
	case err := <-said:
		if err == nil {
			t.Fatal("Say succeeded on a stall")
		}
		if msg := strings.ToLower(err.Error()); strings.Contains(msg, "purple") || strings.Contains(msg, "elephant") {
			t.Fatalf("the error quotes the text to speak: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Say still waiting 3 s")
	}
}
