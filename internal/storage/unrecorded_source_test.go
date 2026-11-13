package storage

import (
	"testing"
	"time"
)

// TestClearDriveReplacementPendingRefusesAMarkedMergedSource fails if a merged
// source may still have an unrecorded Drive folder but the target deletion is
// cleared before discovery searches that source.
func TestClearDriveReplacementPendingRefusesAMarkedMergedSource(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	recordedSession(t, store, "source", started, started.Add(time.Minute), map[float64]string{1: "deleted", 3: "kept"})
	recordedSession(t, store, "other", started.Add(time.Hour), started.Add(61*time.Minute), map[float64]string{1: "other"})
	if err := store.MergeSessions("merged", []string{"source", "other"}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("merged", segments[0].ID, DeletedSpan{AudioPath: "recording-source.mp3", Start: 1, End: 3, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatal(err)
	}
	generation, err := store.DeletionGeneration("merged")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO unrecorded_sync_folders(session_id) VALUES('source')`); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearDriveReplacementPending("merged", generation); err == nil {
		t.Fatal("cleared a deletion while its merged source still needs Drive folder discovery")
	}
}

// TestUnrecordedDriveFolderSessionsIncludesMergedSources fails if replacement
// discovery searches only the deletion owner and misses a marked source.
func TestUnrecordedDriveFolderSessionsIncludesMergedSources(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	recordedSession(t, store, "source", started, started.Add(time.Minute), map[float64]string{1: "source"})
	recordedSession(t, store, "other", started.Add(time.Hour), started.Add(61*time.Minute), map[float64]string{1: "other"})
	if err := store.MergeSessions("merged", []string{"source", "other"}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO unrecorded_sync_folders(session_id) VALUES('source')`); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.UnrecordedDriveFolderSessions("merged")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "source" {
		t.Fatalf("unrecorded folder sessions = %+v, want source", sessions)
	}
}
