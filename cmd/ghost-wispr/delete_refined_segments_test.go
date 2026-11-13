package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestDeleteRefinedTextUsesStoredWordOccurrence(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s1", started.Add(time.Minute), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	words := []storage.RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 0, Text: "Again.", Start: 0, End: 0.5},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 1, Text: "Again.", Start: 1, End: 1.5},
	}
	revision, err := store.PublishRefinementAtGeneration("s1", storage.RefinedTranscript(words), "nova-3", words, storage.BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("control: got %d segments, want two repeated occurrences", len(segments))
	}
	summarized := false
	runner := segmentDeleteRunner{store: store, summarize: func(context.Context, string, time.Time) error {
		summarized = true
		return nil
	}}
	if err := runner.DeleteRefinedText(context.Background(), "s1", segments[0].ID, revision); err != nil {
		t.Fatalf("DeleteRefinedText: %v", err)
	}
	if !summarized {
		t.Fatal("DeleteRefinedText did not regenerate the summary")
	}
	remaining, err := store.RefinedWords("s1", revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Ordinal != 1 {
		t.Fatalf("remaining refined words = %#v, want only the second occurrence", remaining)
	}
}
