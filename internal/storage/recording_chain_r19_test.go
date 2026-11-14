package storage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A source that names itself as the owner of its own recording is the leaf of
// that recording's chain: deleting one of its words keeps its other words.
func TestRecordingChainEndsAtASelfNamedOwner(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "self-owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"s", "t"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("m", []string{"s", "t"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO recording_sources(merged_session_id, recording_path, source_session_id) VALUES ('s', '/audio/s.mp3', 's')`); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "/audio/s.mp3", RecordingSHA256: "s", Ordinal: 0, Text: "first-s.", Start: 0, End: 1},
		{RecordingPath: "/audio/s.mp3", RecordingSHA256: "s", Ordinal: 1, Text: "second-s.", Start: 20, End: 21, Speaker: 1},
	}
	if _, err := store.PublishRefinementAtGeneration("m", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("m")
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	for _, segment := range segments {
		if segment.Text == "first-s." {
			if err := store.DeleteRefinedSegmentText("m", segment.ID, 1); err != nil {
				t.Fatal(err)
			}
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("control: s's first segment is missing: %+v", segments)
	}
	source, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(source.CanonicalTranscript, "first-s") || !strings.Contains(source.CanonicalTranscript, "second-s") {
		t.Fatalf("self-named source canonical = %q, want only its other words", source.CanonicalTranscript)
	}
}
