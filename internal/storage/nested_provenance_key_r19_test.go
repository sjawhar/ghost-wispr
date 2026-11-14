package storage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestR19NestedBareProvenanceKeyReachesLeafSource(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "nested-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"s", "m1", "m2"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET status = 'merged', merged_into = 'm1', canonical_transcript = 'first-s\nsecond-s' WHERE id = 's'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET status = 'merged', merged_into = 'm2', canonical_transcript = 'first-s\nsecond-s' WHERE id = 'm1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO recording_sources(merged_session_id, recording_path, source_session_id) VALUES ('m2', 's.mp3', 'm1'), ('m1', 's.mp3', 's')`); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "/tmp/s.mp3", RecordingSHA256: "s", Ordinal: 0, Text: "first-s", Start: 0, End: 1},
		{RecordingPath: "/tmp/s.mp3", RecordingSHA256: "s", Ordinal: 1, Text: "second-s", Start: 2, End: 3},
	}
	if _, err := store.PublishRefinementAtGeneration("m2", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("m2")
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range segments {
		if segment.Text == "first-s" {
			if err := store.DeleteRefinedSegmentText("m2", segment.ID, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	source, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(source.CanonicalTranscript, "first-s") || !strings.Contains(source.CanonicalTranscript, "second-s") {
		t.Fatalf("nested bare provenance source canonical = %q", source.CanonicalTranscript)
	}
}
