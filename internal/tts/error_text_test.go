package tts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/googleauthtest"
)

// spokenText is the text the providers are asked to speak in the error
// tests: it may be a conversation's words (a summary read out).
const spokenText = "We named the warehouse robot project Purple Elephant Seventeen."

// echoingProviders returns each TTS provider, against a server that answers
// with status 400 and body, which echo writes from the text it was sent.
func echoingProviders(t *testing.T, body func(text string) string) map[string]Provider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Text  string `json:"text"`
			Input struct {
				Text string `json:"text"`
			} `json:"input"`
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		text := request.Text + request.Input.Text
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				text += part.Text
			}
		}
		if text == "" {
			t.Errorf("%s: the request carries no text", r.URL.Path)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body(text)))
	}))
	t.Cleanup(server.Close)

	elevenLabs, err := NewElevenLabsProvider("test-key", "voice", WithElevenLabsBaseURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	google, err := NewGoogleProvider("test-key", "voice", WithGoogleBaseURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := googleauth.Detect(context.Background(), googleauthtest.X509CredentialFile(t, "token"), googleauth.CloudPlatformScope)
	if err != nil {
		t.Fatal(err)
	}
	gemini, err := NewGeminiProvider("project", "model", "Kore", WithGeminiAuthCredentials(credentials), WithGeminiBaseURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Provider{"elevenlabs": elevenLabs, "google": google, "gemini": gemini}
}

// TestATTSErrorDoesNotQuoteTheTextToSpeak: a TTS error is kept in the speak
// request's status (GET /api/tts/status/{id}) and returned by the API. When
// the provider's error quotes the text it was asked to speak, in its message
// or its raw body, the error leaves that out and still says the status.
func TestATTSErrorDoesNotQuoteTheTextToSpeak(t *testing.T) {
	for name, body := range map[string]func(text string) string{
		"in the error message": func(text string) string {
			// A quote of part of the text: from the topic on.
			_, part, found := strings.Cut(text, "project ")
			if !found {
				part = text
			}
			quoted := `Sentence starts with "` + part + `" is too long.`
			payload, _ := json.Marshal(map[string]any{
				"error":  map[string]any{"code": 400, "message": quoted, "status": "INVALID_ARGUMENT"},
				"detail": map[string]any{"status": "invalid_text", "message": quoted},
			})
			return string(payload)
		},
		"in a raw body": func(text string) string { return "bad request for: " + text },
	} {
		t.Run(name, func(t *testing.T) {
			for provider, p := range echoingProviders(t, body) {
				_, err := p.Synthesize(context.Background(), SpeechRequest{Text: spokenText})
				if err == nil {
					t.Fatalf("%s: control: the provider answered 400 and Synthesize did not fail", provider)
				}
				if !strings.Contains(err.Error(), "400") {
					t.Errorf("%s: error %q does not say the status", provider, err)
				}
				for _, word := range []string{"Purple", "Elephant", "Seventeen", "warehouse"} {
					if strings.Contains(err.Error(), word) {
						t.Errorf("%s: error %q quotes the text to speak (%q)", provider, err, word)
					}
				}
			}
		})
	}
}
