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

// TestRecoverInterruptedSyncMarksAnOlderFolderlessSync fails if recovery of a
// pre-upgrade SYNCING row first increments retry_count but never marks the
// unrecorded folder that its later deletion must search for.
func TestRecoverInterruptedSyncMarksAnOlderFolderlessSync(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "legacy-cut-off")
	if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = ?, sync_claim = 'old-claim', sync_claimed_from = ? WHERE id = ?`,
		SyncStateSyncing, SyncStatePending, "legacy-cut-off"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverInterruptedSyncs(5, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var marked int
	if err := store.DB().QueryRow(`SELECT count(*) FROM unrecorded_sync_folders WHERE session_id = ?`, "legacy-cut-off").Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Fatalf("recovered folderless sync marker = %d, want 1", marked)
	}
}
