package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMarkSpliceCompleteAdvancesRevisionAndMatchesOnlyItsRecording(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "splice-revision.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), "/audio/recording.mp3,/audio/not-recording.mp3"); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "/audio/recording.mp3", RecordingSHA256: "before", Ordinal: 0, Text: "cut recording", Start: 3, End: 4},
		{RecordingPath: "/audio/not-recording.mp3", RecordingSHA256: "before", Ordinal: 0, Text: "other recording", Start: 5, End: 6},
	}
	revision, err := store.PublishRefinementAtGeneration("s", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	span := &DeletedSpan{SessionID: "s", AudioPath: "/audio/recording.mp3", Start: 1, End: 2, SHA256Before: "before", SHA256After: "after"}
	if _, err := store.DB().Exec(`INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		span.SessionID, span.AudioPath, span.Start, span.End, span.SHA256Before, span.SHA256After, started.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSpliceComplete(span); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if session.RefinedRevision != revision+1 {
		t.Fatalf("revision after splice = %d, want %d", session.RefinedRevision, revision+1)
	}
	moved, err := store.RefinedWords("s", session.RefinedRevision)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make(map[string]string, len(moved))
	for _, word := range moved {
		hashes[word.RecordingPath] = word.RecordingSHA256
	}
	if len(moved) != 2 || hashes["/audio/recording.mp3"] != "after" || hashes["/audio/not-recording.mp3"] != "before" {
		t.Fatalf("words after splice = %#v, want only recording.mp3 moved", moved)
	}
}
