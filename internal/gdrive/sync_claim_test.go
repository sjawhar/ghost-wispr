package gdrive

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// onFirstUpload runs f as the first upload starts.
func onFirstUpload(drive *fakeDrive, f func()) {
	var fired atomic.Bool
	drive.onUpload = func() {
		if fired.CompareAndSwap(false, true) {
			f()
		}
	}
}

// sessionState returns session id's sync state and Drive folder.
func sessionState(t *testing.T, store *storage.SQLiteStore, id string) (state, folder string) {
	t.Helper()
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	return sess.SyncState, sess.GDriveFolderID
}

// Review of #37, round 4, note 5 (on main too): two syncs of one session could
// run at once (retry-sync, the sync after a summary, the sweep), and with no
// folder yet each created one. A sync now claims the session first; a second
// sync of it, and a manual retry, wait for none and start nothing while the
// first runs.
func TestASecondSyncOfASessionStartsNothingWhileOneRuns(t *testing.T) {
	store := openStore(t)
	const id = "20261106100000"
	summarizedSession(t, store, t.TempDir(), id, time.Now().UTC().Add(-time.Hour), "A summary.")
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)
	ctx := context.Background()

	var canSync, second error
	onFirstUpload(drive, func() {
		canSync = o.CanSync(id)
		second = o.SyncSession(ctx, id)
	})
	if err := o.SyncSession(ctx, id); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if canSync == nil {
		t.Error("CanSync during a sync of the session reported nothing, so retry-sync starts a second one")
	}
	if second != nil {
		t.Errorf("the second sync: %v, want it to start nothing", second)
	}
	if len(drive.folders) != 1 {
		t.Errorf("Drive holds %d folders for the session, want 1", len(drive.folders))
	}
	if state, _ := sessionState(t, store, id); state != storage.SyncStateSynced {
		t.Errorf("after the sync: %q, want synced", state)
	}
}

// Syncs of one session started at the same moment upload it once.
func TestConcurrentSyncsOfOneSessionUploadItOnce(t *testing.T) {
	store := openStore(t)
	const id = "20261106100000"
	summarizedSession(t, store, t.TempDir(), id, time.Now().UTC().Add(-time.Hour), "A summary.")
	drive := newFakeDrive()
	drive.onUpload = func() { time.Sleep(20 * time.Millisecond) } // a slow upload
	o := NewOrchestratorWithUploader(drive, store)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := o.SyncSession(context.Background(), id); err != nil {
				t.Errorf("sync: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	drive.mu.Lock()
	folders, uploaded := len(drive.folders), drive.uploaded
	drive.mu.Unlock()
	if folders != 1 || uploaded != 3 {
		t.Errorf("Drive holds %d folders after %d uploads, want 1 folder and 3 uploads (summary, transcript, recording)", folders, uploaded)
	}
	if state, _ := sessionState(t, store, id); state != storage.SyncStateSynced {
		t.Errorf("after the syncs: %q, want synced", state)
	}
}

// Review of #37, round 4, note 5 (red-team N2): MarkSynced checked no sync
// state, so a sync marked synced a session that had left its hands during the
// upload (here its Drive folder was found deleted by hand). It now marks only
// a session the sync still holds.
func TestASyncMarksSyncedOnlyASessionItStillHolds(t *testing.T) {
	store := openStore(t)
	const id = "20261106100000"
	summarizedSession(t, store, t.TempDir(), id, time.Now().UTC().Add(-time.Hour), "A summary.")
	drive := newFakeDrive()
	onFirstUpload(drive, func() {
		// The app restarted, so the session is no longer this sync's, and
		// the sync that took it found its Drive folder deleted by hand.
		if _, err := store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC()); err != nil {
			t.Errorf("recover interrupted syncs: %v", err)
		}
		syncTo(t, store, id, storage.SyncStateRemoteDeleted, "", 0, "the Drive folder was deleted by hand")
	})

	syncErr := NewOrchestratorWithUploader(drive, store).SyncSession(context.Background(), id)
	if state, _ := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted {
		t.Errorf("after the sync (%v): %q, want %q left as it was", syncErr, state, storage.SyncStateRemoteDeleted)
	}
}

// Review of #37, round 5, note 3 (red-team N2): a sync's failure write used
// its own idea of the state, so a stale sync that failed replaced a correct
// SYNCED with a retry or a failure. The failure write now applies only while
// the sync still holds the session.
func TestAFailedSyncNeverReplacesTheStateItNoLongerHolds(t *testing.T) {
	store := openStore(t)
	const id = "20261106100000"
	summarizedSession(t, store, t.TempDir(), id, time.Now().UTC().Add(-time.Hour), "A summary.")
	drive := newFakeDrive()
	drive.failAfter = 1
	onFirstUpload(drive, func() {
		// The app restarted and another sync, which found the session no
		// longer held, finished and marked it synced.
		if _, err := store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC()); err != nil {
			t.Errorf("recover interrupted syncs: %v", err)
		}
		syncTo(t, store, id, storage.SyncStateSynced, "folder-of-the-other-sync", 0, "")
	})

	syncErr := NewOrchestratorWithUploader(drive, store).SyncSession(context.Background(), id)
	if syncErr == nil {
		t.Fatal("the cut-off sync succeeded")
	}
	if state, folder := sessionState(t, store, id); state != storage.SyncStateSynced || folder != "folder-of-the-other-sync" {
		t.Errorf("after the failed sync (%v): %q in folder %q, want synced in folder-of-the-other-sync", syncErr, state, folder)
	}
}

// Review of #37, round 5, note 4 (red-team N3): an error while marking a
// session synced returned before any state write, so the session stayed mid
// sync for good: the sweep never syncs a SYNCING session. It now goes back to
// a retry, keeping its folder, and the retry syncs it there.
func TestAnErrorWhileMarkingSyncedSchedulesARetry(t *testing.T) {
	store := openStore(t)
	const id = "20261106100000"
	summarizedSession(t, store, t.TempDir(), id, time.Now().UTC().Add(-time.Hour), "A summary.")
	// The database refuses the write that marks a session synced.
	if _, err := store.DB().Exec(`CREATE TRIGGER refuse_synced BEFORE UPDATE OF sync_state ON sessions
		WHEN new.sync_state = 'SYNCED' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)

	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Fatal("the sync succeeded though marking it synced failed")
	}
	state, folder := sessionState(t, store, id)
	if state != storage.SyncStateRetryScheduled || folder == "" {
		t.Fatalf("after the error: %q in folder %q, want %q in the folder the sync made", state, folder, storage.SyncStateRetryScheduled)
	}

	if _, err := store.DB().Exec(`DROP TRIGGER refuse_synced`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if state, after := sessionState(t, store, id); state != storage.SyncStateSynced || after != folder {
		t.Errorf("after the retry: %q in folder %q, want synced in %s", state, after, folder)
	}
	if len(drive.folders) != 1 {
		t.Errorf("Drive holds %d folders for the session, want 1", len(drive.folders))
	}
}
