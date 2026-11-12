package transcribe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBatchRefinement_DeepgramSubmissionAndTranscriptExtraction(t *testing.T) {
	tempDir := t.TempDir()
	audioPath := filepath.Join(tempDir, "session.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0o644); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}

	var gotAuth string
	var gotBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST request, got %s", r.Method)
		}
		if r.URL.Path != "/v1/listen" {
			t.Fatalf("expected /v1/listen path, got %s", r.URL.Path)
		}
		if gotModel := r.URL.Query().Get("model"); gotModel != "nova-3" {
			t.Fatalf("expected model query nova-3, got %q", gotModel)
		}
		gotAuth = r.Header.Get("Authorization")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		_, _ = w.Write([]byte(`{"metadata":{"models":["provider-id"],"model_info":{"provider-id":{"name":"general-nova","arch":"nova-3"}}},"results":{"channels":[{"alternatives":[{"transcript":"refined transcript"}]}]}}`))
	}))
	defer server.Close()

	client := NewDeepgramBatchTranscriber(DeepgramBatchConfig{
		APIKey:  "test-key",
		Model:   "nova-3",
		BaseURL: server.URL,
	})

	got, err := client.Transcribe(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Transcribe returned error: %v", err)
	}
	if got.Transcript != "refined transcript" {
		t.Fatalf("expected refined transcript, got %q", got.Transcript)
	}
	if gotAuth != "Token test-key" {
		t.Fatalf("expected authorization header %q, got %q", "Token test-key", gotAuth)
	}
	if string(gotBody) != "fake-audio" {
		t.Fatalf("expected raw audio body %q, got %q", "fake-audio", string(gotBody))
	}
}

func TestBatchReturnsWordTimestamps(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "deepgram", "prerecorded_words.json"))
	if err != nil {
		t.Fatalf("read Deepgram response fixture: %v", err)
	}

	apiKey := "test-key"

	audioPath := filepath.Join(t.TempDir(), "session.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0o644); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	client := NewDeepgramBatchTranscriber(DeepgramBatchConfig{
		APIKey:  apiKey,
		Model:   "nova-3",
		BaseURL: server.URL,
	})
	words, err := client.Words(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Words returned error: %v", err)
	}

	want := []TimestampedWord{
		{Text: "Hello,", Start: 0.12, End: 0.48},
		{Text: "office", Start: 0.53, End: 0.91},
		{Text: "recorder.", Start: 0.96, End: 1.62},
	}
	if len(words) != len(want) {
		t.Fatalf("word count = %d, want %d", len(words), len(want))
	}
	for i, expected := range want {
		if words[i] != expected {
			t.Errorf("word %d = %#v, want %#v", i, words[i], expected)
		}
	}

	result, err := client.Transcribe(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Transcribe returned error: %v", err)
	}
	if result.Transcript != "Hello, office recorder." {
		t.Fatalf("Transcribe returned %q, want fixture transcript", result.Transcript)
	}
	if result.Model != "nova-3" {
		t.Fatalf("Transcribe reported model %q, want the fixture's nova-3", result.Model)
	}
}

func TestBatchWordsFallsBackToRawWord(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "session.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0o644); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":{"channels":[{"alternatives":[{"words":[{"word":"fallback","start":0.2,"end":0.8}]}]}]}}`))
	}))
	defer server.Close()

	client := NewDeepgramBatchTranscriber(DeepgramBatchConfig{BaseURL: server.URL})
	words, err := client.Words(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Words returned error: %v", err)
	}
	if len(words) != 1 || words[0] != (TimestampedWord{Text: "fallback", Start: 0.2, End: 0.8}) {
		t.Fatalf("Words = %#v, want the raw-word fallback", words)
	}
}

func TestBatchRefinement_DeepgramNonOKFails(t *testing.T) {
	tempDir := t.TempDir()
	audioPath := filepath.Join(tempDir, "session.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0o644); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewDeepgramBatchTranscriber(DeepgramBatchConfig{
		APIKey:  "test-key",
		Model:   "nova-3",
		BaseURL: server.URL,
	})

	_, err := client.Transcribe(context.Background(), audioPath)
	if err == nil {
		t.Fatal("expected error when Deepgram returns non-200")
	}
}

func TestBatchRefinementReportsDeepgramResponseModel(t *testing.T) {
	const (
		configuredModel = "configured-refinement-model"
		providerModel   = "provider-refinement-model"
	)
	audioPath := filepath.Join(t.TempDir(), "session.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0o644); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{
				"models": []string{"provider-id"},
				"model_info": map[string]any{
					"provider-id": map[string]string{"name": "provider-internal-name", "arch": providerModel},
				},
			},
			"results": map[string]any{
				"channels": []any{map[string]any{
					"alternatives": []any{map[string]string{"transcript": "refined transcript"}},
				}},
			},
		})
	}))
	t.Cleanup(server.Close)

	client := NewDeepgramBatchTranscriber(DeepgramBatchConfig{Model: configuredModel, BaseURL: server.URL})
	result, err := client.Transcribe(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if result.Model != providerModel {
		t.Fatalf("reported model = %q, want provider value %q, not configured %q", result.Model, providerModel, configuredModel)
	}
}
