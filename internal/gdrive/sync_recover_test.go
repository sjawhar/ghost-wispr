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
	if err := o.CanSync(id); err == nil {
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
