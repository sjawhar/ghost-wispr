package tts

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// exampleLabels are a provider's request labels in the tests.
var exampleLabels = map[string]string{"owner": "example-owner", "stream": "example-stream"}

// requestLabels reads a streamGenerateContent request body's labels, and
// whether it had any.
func requestLabels(t *testing.T, raw []byte) (map[string]string, bool) {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("request body %s: %v", raw, err)
	}
	field, has := body["labels"]
	var labels map[string]string
	if has {
		if err := json.Unmarshal(field, &labels); err != nil {
			t.Fatalf("request labels %s: %v", field, err)
		}
	}
	return labels, has
}

// TestGeminiTTSOnVertexAICarriesTheProvidersLabels fails if voice A's
// streamGenerateContent on a Vertex AI provider does not carry the
// provider's labels, which Vertex AI copies into Cloud Billing.
func TestGeminiTTSOnVertexAICarriesTheProvidersLabels(t *testing.T) {
	rt := &recordingTransport{pcm: ramp(0, 960)}
	p := vertexAt("")
	p.Labels = exampleLabels
	g, err := NewEngine(testProvider(t, p, nil), "gemini-3.8-flash-tts", "", WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := g.Stream(context.Background(), "Twelve open issues.", func([]byte) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if labels, _ := requestLabels(t, rt.body); !maps.Equal(labels, exampleLabels) {
		t.Fatalf("the request carried labels %v, want the provider's %v", labels, exampleLabels)
	}
	if text, voice := geminiRequestTextAndVoice(t, rt.body); text != "Twelve open issues." || voice != "Kore" {
		t.Fatalf("request text %q in voice %q, want the text in Kore", text, voice)
	}
}

// TestGeminiTTSOnTheGeminiAPICarriesNoLabels fails if Gemini TTS on a
// Gemini API provider, such as a model gateway's Google routes, carries
// labels, even when its entry names some: only Vertex AI takes them.
func TestGeminiTTSOnTheGeminiAPICarriesNoLabels(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStream(r) {
			http.NotFound(w, r)
			return
		}
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseAudio(ramp(0, 960)))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, &config.Provider{API: config.APIGemini, URL: srv.URL, Auth: keyAuth(), Labels: exampleLabels}, nil)
	g, err := NewEngine(p, "gemini-3.8-flash-tts", "")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := g.Stream(context.Background(), "Three are blocked.", func([]byte) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if labels, has := requestLabels(t, body); has {
		t.Fatalf("the request carried labels %v, want none", labels)
	}
}
