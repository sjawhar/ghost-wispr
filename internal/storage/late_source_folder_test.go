package storage

import (
	"testing"
	"time"
)

// TestRecordSyncFolderOwesAMergedTargetsDeletion fails if a source creates its
// Drive folder after the merged target's deletion made a non-empty ledger: the
// source folder can hold the deleted words and must be added before the
// replacement finishes.
func TestRecordSyncFolderOwesAMergedTargetsDeletion(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	recordedSession(t, store, "source", started, started.Add(time.Minute), map[float64]string{1: "deleted", 3: "kept"})
	recordedSession(t, store, "other", started.Add(time.Hour), started.Add(61*time.Minute), map[float64]string{1: "other"})
	if err := store.MergeSessions("merged", []string{"source", "other"}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimSync("source", SyncStatePending, "source-sync", time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("ClaimSync source = %v, %v", claimed, err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedAudioTestSegment(t, store, "merged", segments[0].ID, DeletedSpan{AudioPath: "recording-source.mp3", Start: 1, End: 3, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatal(err)
	}
	if recorded, err := store.RecordSyncFolder("source", "source-sync", "source-folder"); err != nil || !recorded {
		t.Fatalf("RecordSyncFolder source = %v, %v", recorded, err)
	}
	generation, err := store.DeletionGeneration("merged")
	if err != nil {
		t.Fatal(err)
	}
	folders, err := store.DriveReplacementFolders("merged", generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].FolderSessionID != "source" || folders[0].State != DriveReplacementFolderOwed {
		t.Fatalf("merged replacement folders = %+v, want the source folder owed", folders)
	}
}
