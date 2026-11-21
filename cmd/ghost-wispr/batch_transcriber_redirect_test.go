package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestBatchTranscriptionSendsItsKeyOnlyToItsProvider fails if the batch
// transcriber the unit builds follows a redirect from its Deepgram provider to
// another origin with the key and the recording, as net/http's own client
// does for a host that differs only by its port.
func TestBatchTranscriptionSendsItsKeyOnlyToItsProvider(t *testing.T) {
	var elsewhereSaw atomic.Value
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		elsewhereSaw.Store(r.Header.Get("Authorization") + "|" + string(body))
		_, _ = io.WriteString(w, `{"metadata":{"model_info":{}},"results":{"channels":[]}}`)
	}))
	defer elsewhere.Close()
	deepgram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer deepgram.Close()

	recording := filepath.Join(t.TempDir(), "recording.wav")
	if err := os.WriteFile(recording, wav24k([]byte("synthetic-audio-planted")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := deepgramKeyConfig("dg-1")
	entry := cfg.Providers["deepgram"]
	entry.URL = deepgram.URL
	cfg.Providers["deepgram"] = entry
	cfg.BatchTranscription = config.BatchTranscription{Model: "deepgram/nova-3"}

	transcriber, err := makeBatchTranscriber(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcriber.Transcribe(context.Background(), recording); err == nil {
		t.Error("a transcription redirected to another origin succeeded, want an error")
	}
	if saw, _ := elsewhereSaw.Load().(string); saw != "" {
		t.Fatalf("the other origin received %q; want nothing of the key or the recording", saw)
	}
}
