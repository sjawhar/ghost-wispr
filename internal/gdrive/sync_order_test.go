package gdrive

import (
	"context"
	"testing"
	"time"
)

// TestRealSyncerRecordsNewFolderBeforeItsFirstUpload fails if Syncer creates
// a folder and starts uploading documents before the orchestrator has stored
// the folder ID under its sync claim.
func TestRealSyncerRecordsNewFolderBeforeItsFirstUpload(t *testing.T) {
	store := openStore(t)
	const id = "20261106140000"
	summarizedSession(t, store, t.TempDir(), id, time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC), "summary")
	wire, syncer := newWireDrive(t)
	seenUpload := false
	wire.onUpload = func(folderID string) {
		seenUpload = true
		session, err := store.GetSession(id)
		if err != nil {
			t.Errorf("get session while its first upload starts: %v", err)
			return
		}
		if session.GDriveFolderID != folderID {
			t.Errorf("folder ID at first upload = %q, want %q", session.GDriveFolderID, folderID)
		}
	}
	if err := NewOrchestrator(syncer, store).SyncSession(context.Background(), id); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !seenUpload {
		t.Fatal("control: the real Syncer did not upload a file")
	}
}
