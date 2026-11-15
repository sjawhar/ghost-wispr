package transcribe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestBatchTranscribeReturnsItsWordTimes fails if a refinement's
// transcription comes back without its words and their times: refinement
// rebuilds the transcript from them to leave out the words "Delete text"
// removed.
func TestBatchTranscribeReturnsItsWordTimes(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "deepgram", "prerecorded_words.json"))
	if err != nil {
		t.Fatalf("read Deepgram response fixture: %v", err)
	}
	audioPath := filepath.Join(t.TempDir(), "session.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	result, err := NewDeepgramBatchTranscriber(DeepgramBatchConfig{Model: "nova-3", BaseURL: server.URL}).Transcribe(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	want := []TimestampedWord{
		{Text: "Hello,", Start: 0.12, End: 0.48},
		{Text: "office", Start: 0.53, End: 0.91},
		{Text: "recorder.", Start: 0.96, End: 1.62},
	}
	if len(result.Words) != len(want) {
		t.Fatalf("Transcribe returned %d words, want %d: %+v", len(result.Words), len(want), result.Words)
	}
	for i := range want {
		if result.Words[i] != want[i] {
			t.Errorf("word %d = %+v, want %+v", i, result.Words[i], want[i])
		}
	}
}
