package transcribe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		_, _ = w.Write([]byte(`{"results":{"channels":[{"alternatives":[{"transcript":"refined transcript"}]}]}}`))
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
	if got != "refined transcript" {
		t.Fatalf("expected refined transcript, got %q", got)
	}
	if gotAuth != "Token test-key" {
		t.Fatalf("expected authorization header %q, got %q", "Token test-key", gotAuth)
	}
	if string(gotBody) != "fake-audio" {
		t.Fatalf("expected raw audio body %q, got %q", "fake-audio", string(gotBody))
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

func TestDeepgramBatchTranscriber_Keywords(t *testing.T) {
	expectedKeywords := []string{"Taiga", "Anthropic"}
	var capturedURL string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":{"channels":[{"alternatives":[{"transcript":"test"}]}]}}`)
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "session.mp3")
	if err := os.WriteFile(audioPath, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	transcriber := NewDeepgramBatchTranscriber(DeepgramBatchConfig{
		APIKey:     "test-key",
		Model:      "nova-3",
		BaseURL:    ts.URL,
		Keywords:   expectedKeywords,
		HTTPClient: ts.Client(),
	})

	_, err := transcriber.Transcribe(context.Background(), audioPath)
	if err != nil {
		t.Fatal(err)
	}

	parsed, _ := url.Parse(capturedURL)
	gotKeywords := parsed.Query()["keywords"]
	if len(gotKeywords) != 2 || gotKeywords[0] != "Taiga" || gotKeywords[1] != "Anthropic" {
		t.Fatalf("expected keywords [Taiga Anthropic], got %v", gotKeywords)
	}
}
