package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteRefinedBatchOnlyFiller(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "filler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "h", Ordinal: 0, Text: "Before.", Start: 0, End: 1},
		{RecordingPath: "recording.mp3", RecordingSHA256: "h", Ordinal: 1, Text: "Um,", Start: 2, End: 2.2},
		{RecordingPath: "recording.mp3", RecordingSHA256: "h", Ordinal: 2, Text: "After.", Start: 3, End: 4},
	}
	revision, err := store.PublishRefinementAtGeneration("s", RefinedTranscript(words), "nova-3", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, _ := store.GetSegments("s")
	if err := store.DeleteRefinedSegmentText("s", segments[1].ID, revision); err != nil {
		t.Fatal(err)
	}
	session, _ := store.GetSession("s")
	remaining, err := store.RefinedWords("s", session.RefinedRevision)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].Text != "Before." || remaining[1].Text != "After." {
		t.Fatalf("remaining words = %#v, want only batch-only filler removed", remaining)
	}
}

func TestDeleteRefinedRepeatedFillerKeepsOtherOccurrence(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "repeat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "h", Ordinal: 0, Text: "Um,", Start: 0, End: 0.2},
		{RecordingPath: "recording.mp3", RecordingSHA256: "h", Ordinal: 1, Text: "Um,", Start: 1, End: 1.2},
	}
	revision, err := store.PublishRefinementAtGeneration("s", RefinedTranscript(words), "nova-3", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, _ := store.GetSegments("s")
	if err := store.DeleteRefinedSegmentText("s", segments[1].ID, revision); err != nil {
		t.Fatal(err)
	}
	session, _ := store.GetSession("s")
	remaining, err := store.RefinedWords("s", session.RefinedRevision)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Ordinal != 0 || remaining[0].Text != "Um," {
		t.Fatalf("remaining words = %#v, want first repeated filler retained", remaining)
	}
}
