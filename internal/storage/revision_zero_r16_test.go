package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRevisionZeroDeleteTextRefusesTimedSegment(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "revision-zero.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), "/audio/s.mp3"); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{{RecordingPath: "/audio/s.mp3", RecordingSHA256: "s", Ordinal: 0, Text: "timed words", Start: 0, End: 1}}
	if _, err := store.PublishRefinementAtGeneration("s", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("timed segments = %#v", segments)
	}
	if err := store.DeleteRefinedSegmentText("s", segments[0].ID, 0); !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("revision-zero timed delete = %v, want segment refusal", err)
	}
	segments, err = store.GetSegments("s")
	if err != nil || len(segments) != 1 || segments[0].Text != "timed words" {
		t.Fatalf("timed segment after refused revision-zero delete = %#v, %v", segments, err)
	}
}
