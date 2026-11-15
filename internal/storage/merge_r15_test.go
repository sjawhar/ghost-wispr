package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMergeMarksOnlyNeverRecordedSegmentsUntimedAndKeepsRemovedProvenance(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "merge-r15.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("never", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("never", transcribeSegment("never recorded", 0, started)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("never", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("removed", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("removed", started.Add(61*time.Minute), "/audio/removed.mp3"); err != nil {
		t.Fatal(err)
	}
	removedWords := []RefinedWord{{RecordingPath: "/audio/removed.mp3", RecordingSHA256: "removed", Ordinal: 0, Text: "recording was removed", Start: 0, End: 1}}
	if _, err := store.PublishRefinementAtGeneration("removed", RefinedTranscript(removedWords), "test", removedWords, BuildRefinedSegments(removedWords), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z' WHERE id = 'removed'`); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("available", started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("available", started.Add(121*time.Minute), "/audio/available.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"never", "removed", "available"}, started, started.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	untimedNever, untimedRemoved := false, false
	for _, segment := range segments {
		switch segment.Text {
		case "never recorded":
			untimedNever = segment.Untimed && segment.TranscriptRevision == 0 && segment.RecordingPath == ""
		case "recording was removed":
			untimedRemoved = segment.Untimed
		}
	}
	if !untimedNever {
		t.Fatal("never-recorded source segment was not reset as untimed")
	}
	if untimedRemoved {
		t.Fatal("GC-removed recording was marked untimed")
	}
	var sources int
	if err := store.DB().QueryRow(`SELECT count(*) FROM recording_sources WHERE merged_session_id = 'merged' AND recording_path = '/audio/removed.mp3' AND source_session_id = 'removed'`).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if sources != 1 {
		t.Fatalf("removed recording provenance rows = %d, want 1", sources)
	}
}
