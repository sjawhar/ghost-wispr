package gdrive

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// Review of #37, round 4, note 5 (red-team N7): a sync cut off by the app
// stopping left its session SYNCING for good. At startup every such session
// goes back to a retry, keeping its folder, and the retry syncs it there.
func TestAnInterruptedSyncIsRetriedInItsFolderAfterARestart(t *testing.T) {
	store := openStore(t)
	const id = "20261106100000"
	summarizedSession(t, store, t.TempDir(), id, time.Now().UTC().Add(-time.Hour), "A summary.")
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)
	ctx := context.Background()

	// The first sync makes the folder and is cut off.
	drive.failAfter = 2
	if err := o.SyncSession(ctx, id); err == nil {
		t.Fatal("the cut-off first sync succeeded")
	}
	drive.failAfter = 0
	_, folder := sessionState(t, store, id)

	// The retry is under way when the app stops.
	onFirstUpload(drive, func() { panic("the app stops") })
	func() {
		defer func() { _ = recover() }()
		_ = o.SyncSession(ctx, id)
	}()
	drive.onUpload = nil
	if state, _ := sessionState(t, store, id); state != storage.SyncStateSyncing {
		t.Fatalf("after the stop: %q, want the session left mid-sync", state)
	}

	// The app starts again.
	recovered, err := store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC())
	if err != nil {
		t.Fatalf("recover interrupted syncs: %v", err)
	}
	if len(recovered) != 1 || recovered[0] != id {
		t.Errorf("recovered %v, want [%s]", recovered, id)
	}
	if state, after := sessionState(t, store, id); state != storage.SyncStateRetryScheduled || after != folder {
		t.Fatalf("after the restart: %q in folder %q, want %q in %s", state, after, storage.SyncStateRetryScheduled, folder)
	}
	if err := o.SyncSession(ctx, id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if state, after := sessionState(t, store, id); state != storage.SyncStateSynced || after != folder {
		t.Errorf("after the retry: %q in folder %q, want synced in %s", state, after, folder)
	}
	if len(drive.folders) != 1 {
		t.Errorf("Drive holds %d folders for the session, want 1", len(drive.folders))
	}
}

// TestFirstSyncCutOffAfterFolderCreationReusesTheFolderAndOwesIt fails if a
// power loss after Drive creates a first folder leaves its id only in memory:
// the retry would make another folder, and a later segment deletion would
// leave the first one out of the replacement ledger.
func TestFirstSyncCutOffAfterFolderCreationReusesTheFolderAndOwesIt(t *testing.T) {
	store := openStore(t)
	const id = "20261106110000"
	_, segments := round2Session(t, store, id, []string{"kept", "deleted"})
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)

	drive.onFolderCreated = func() { panic("power loss") }
	stopped := false
	func() {
		defer func() {
			stopped = recover() != nil
		}()
		_ = o.SyncSession(context.Background(), id)
	}()
	if !stopped {
		t.Fatal("the first sync did not stop after creating its Drive folder")
	}
	state, folder := sessionState(t, store, id)
	if state != storage.SyncStateSyncing || folder == "" {
		t.Fatalf("after the cut-off: state=%q folder=%q, want syncing with the created folder", state, folder)
	}

	drive.onFolderCreated = nil
	if _, err := store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if state, after := sessionState(t, store, id); state != storage.SyncStateSynced || after != folder {
		t.Fatalf("after the retry: state=%q folder=%q, want synced in the first folder %q", state, after, folder)
	}
	if len(drive.folders) != 1 {
		t.Fatalf("Drive folders = %d, want the first folder reused", len(drive.folders))
	}

	span := storage.DeletedSpan{SessionID: id, AudioPath: "recording.mp3", Start: 1, End: 2, SHA256Before: "before", SHA256After: "after"}
	if err := deleteRefinedAudioForTest(t, store, id, segments[1].ID, &span); err != nil {
		t.Fatalf("delete refined segment: %v", err)
	}
	generation, err := store.DeletionGeneration(id)
	if err != nil {
		t.Fatal(err)
	}
	owed, err := store.DriveReplacementFolders(id, generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(owed) != 1 || owed[0].FolderSessionID != id || owed[0].State != storage.DriveReplacementFolderOwed {
		t.Fatalf("replacement ledger = %+v, want the first folder owed", owed)
	}
}

// A sync whose every closing write fails (the database refuses them) leaves
// its session SYNCING: no other sync takes it meanwhile, and the next start
// of the app sends it to a retry in its folder.
func TestASyncThatCannotRecordItsEndIsRetriedAfterARestart(t *testing.T) {
	store := openStore(t)
	const id = "20261106100000"
	summarizedSession(t, store, t.TempDir(), id, time.Now().UTC().Add(-time.Hour), "A summary.")
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)
	ctx := context.Background()

	// The first sync makes the folder and is cut off.
	drive.failAfter = 2
	if err := o.SyncSession(ctx, id); err == nil {
		t.Fatal("the cut-off first sync succeeded")
	}
	drive.failAfter = 0
	_, folder := sessionState(t, store, id)

	// The database refuses every sync-state write but the claim.
	if _, err := store.DB().Exec(`CREATE TRIGGER refuse_sync_end BEFORE UPDATE OF sync_state ON sessions
		WHEN new.sync_state != 'SYNCING' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := o.SyncSession(ctx, id); err == nil {
		t.Fatal("the retry succeeded though no closing write could be made")
	}
	if state, _ := sessionState(t, store, id); state != storage.SyncStateSyncing {
		t.Fatalf("after the retry: %q, want the session left SYNCING", state)
	}
	if err := o.CanSync(context.Background(), id); err == nil {
		t.Error("CanSync of a session a sync still holds reported nothing")
	}

	// The app starts again, with the database working.
	if _, err := store.DB().Exec(`DROP TRIGGER refuse_sync_end`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC()); err != nil {
		t.Fatalf("recover interrupted syncs: %v", err)
	}
	if state, after := sessionState(t, store, id); state != storage.SyncStateRetryScheduled || after != folder {
		t.Fatalf("after the restart: %q in folder %q, want %q in %s", state, after, storage.SyncStateRetryScheduled, folder)
	}
	if err := o.SyncSession(ctx, id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if state, after := sessionState(t, store, id); state != storage.SyncStateSynced || after != folder {
		t.Errorf("after the retry: %q in folder %q, want synced in %s", state, after, folder)
	}
	if len(drive.folders) != 1 {
		t.Errorf("Drive holds %d folders for the session, want 1", len(drive.folders))
	}
}
