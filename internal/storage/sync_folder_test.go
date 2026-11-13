package storage

import (
	"testing"
	"time"
)

// TestRecordSyncFolderOwesAPendingDeletion fails if a sync creates a folder
// after a segment deletion started and records it without adding it to the
// deletion's replacement ledger.
func TestRecordSyncFolderOwesAPendingDeletion(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "folder-after-delete")
	segments, err := store.GetSegments("folder-after-delete")
	if err != nil {
		t.Fatal(err)
	}
	const claim = "sync-claim"
	if _, claimed, err := store.ClaimSync("folder-after-delete", SyncStatePending, claim, time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("ClaimSync = %v, %v", claimed, err)
	}
	if err := store.DeleteSegment("folder-after-delete", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 1, End: 2, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatal(err)
	}
	if recorded, err := store.RecordSyncFolder("folder-after-delete", claim, "created-after-delete"); err != nil || !recorded {
		t.Fatalf("RecordSyncFolder = %v, %v", recorded, err)
	}
	generation, err := store.DeletionGeneration("folder-after-delete")
	if err != nil {
		t.Fatal(err)
	}
	folders, err := store.DriveReplacementFolders("folder-after-delete", generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].FolderSessionID != "folder-after-delete" || folders[0].State != DriveReplacementFolderOwed {
		t.Fatalf("replacement folders = %+v, want the created folder owed", folders)
	}
}
