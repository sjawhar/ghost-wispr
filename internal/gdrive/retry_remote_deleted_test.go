package gdrive

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// remoteDeletedSession syncs an old session, moves its folder to the trash
// and syncs a change, so the session ends REMOTE_DELETED with its folder in
// the trash. It returns the session, its folder and its audio directory.
func remoteDeletedSession(t *testing.T, wire *wireDrive, store *storage.SQLiteStore, o *Orchestrator) (id, folder, audioDir string) {
	t.Helper()
	audioDir = t.TempDir()
	id = "20260901100000"
	summarizedSession(t, store, audioDir, id, time.Now().UTC().Add(-60*24*time.Hour), "The summary.")
	folder = syncedSession(t, store, o, id)
	wire.trash(folder)
	if err := store.UpdateTitle(id, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	_ = o.SyncSession(context.Background(), id)
	if state, _ := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted {
		t.Fatalf("after the sync into the trash: %q, want %q", state, storage.SyncStateRemoteDeleted)
	}
	return id, folder, audioDir
}

// expectKeptAsRemoteDeleted fails the test unless session id is still
// REMOTE_DELETED in folder, and Drive confirms the folder remains in the
// trash, so the device keeps its only lasting copy.
func expectKeptAsRemoteDeleted(t *testing.T, store *storage.SQLiteStore, drive *Orchestrator, id, folder, audioDir string) {
	t.Helper()
	if state, kept := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted || kept != folder {
		t.Errorf("session %q in %q, want %q in %q: Drive never showed its folder back", state, kept, storage.SyncStateRemoteDeleted, folder)
	}
	deleted, err := gc.New(store, gc.Config{MaxAgeDays: 30, AudioDir: audioDir, Drive: drive}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("GC deleted %v, whose Drive folder is in the trash", deleted)
	}
}

// Review of #39, blocker 1: a retry of a REMOTE_DELETED session claimed it
// before it asked Drive. When Drive did not answer, the retry ended
// RETRY_SCHEDULED and GC then deleted the session, its folder still in the
// trash. A retry now asks Drive first, and changes nothing until Drive shows
// the folder back.
func TestARetryDuringADriveOutageLeavesARemoteDeletedSessionAsItIs(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	id, folder, audioDir := remoteDeletedSession(t, wire, store, o)

	wire.mu.Lock()
	wire.unavailable = true
	wire.mu.Unlock()
	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Error("retry during a Drive outage: no error")
	}

	expectKeptAsRemoteDeleted(t, store, o, id, folder, audioDir)
}

// Review of #39, blocker 1: the app stopping during a retry of a
// REMOTE_DELETED session left it SYNCING, and startup recovery moved it to
// RETRY_SCHEDULED. The retry now holds the session only once Drive shows its
// folder back: stopped while it asks Drive, it leaves the session
// REMOTE_DELETED.
func TestTheAppStoppingDuringARetryLeavesARemoteDeletedSessionAsItIs(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	id, folder, audioDir := remoteDeletedSession(t, wire, store, o)

	// The app stops (and starts again) as the retry asks Drive about the
	// folder: startup recovers every session left mid-sync.
	stopped := false
	wire.mu.Lock()
	wire.onGet = func(got string) {
		if got != folder || stopped {
			return
		}
		stopped = true
		if _, err := store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC()); err != nil {
			t.Errorf("recover interrupted syncs: %v", err)
		}
	}
	wire.mu.Unlock()
	_ = o.SyncSession(context.Background(), id)
	if !stopped {
		t.Fatal("the retry never asked Drive about the folder")
	}

	expectKeptAsRemoteDeleted(t, store, o, id, folder, audioDir)
}

// Review of #39, round 2, note 1: Drive shows the folder back, the retry
// holds the session, and then the user moves the folder to the trash again
// and the app stops during the sync. Startup recovery puts the session back
// to REMOTE_DELETED, not to a retry, so GC keeps it.
func TestTheAppStoppingAfterARetryHeldTheSessionLeavesItRemoteDeleted(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	id, folder, audioDir := remoteDeletedSession(t, wire, store, o)
	wire.mu.Lock()
	delete(wire.trashed, folder)
	asked := 0
	// The first question about the folder is the retry's, before it holds
	// the session; the second is the upload's, after it does.
	wire.onGet = func(got string) {
		if got != folder {
			return
		}
		asked++
		if asked != 2 {
			return
		}
		wire.trashLocked(folder)
		if _, err := store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC()); err != nil {
			t.Errorf("recover interrupted syncs: %v", err)
		}
	}
	wire.mu.Unlock()

	_ = o.SyncSession(context.Background(), id)
	if asked < 2 {
		t.Fatalf("Drive was asked about the folder %d times, want the retry's and the upload's", asked)
	}
	expectKeptAsRemoteDeleted(t, store, o, id, folder, audioDir)
}

// Review of #39, note 1: Drive answered 404 for a file of the session's
// folder that went between the list and its update, and the sync took that
// for the folder gone and marked the session REMOTE_DELETED. The sync now
// asks Drive about the folder again: it is there, so the sync is retried,
// and the retry makes the document again.
func TestADocumentGoneDuringASyncIsNotTheFolderGone(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	const id = "20261106090000"
	summarizedSession(t, store, t.TempDir(), id, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC), "The summary.")
	folder := syncedSession(t, store, o, id)
	if err := store.UpdateTitle(id, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// summary.md goes from Drive just after the sync lists the folder.
	wire.mu.Lock()
	wire.onList = func(listed string) {
		if listed != folder || wire.onList == nil {
			return
		}
		wire.onList = nil
		for i, f := range wire.files {
			if f.Parent == folder && f.Name == "summary.md" {
				wire.files = append(wire.files[:i], wire.files[i+1:]...)
				break
			}
		}
	}
	wire.mu.Unlock()

	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Error("sync whose document went during it: no error")
	}
	if state, kept := sessionState(t, store, id); state != storage.SyncStateRetryScheduled || kept != folder {
		t.Errorf("after the sync: %q in %q, want %q in %q: the folder is on Drive", state, kept, storage.SyncStateRetryScheduled, folder)
	}

	// The retry makes summary.md again, in the same folder.
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if state, _ := sessionState(t, store, id); state != storage.SyncStateSynced {
		t.Errorf("after the retry: %q, want %q", state, storage.SyncStateSynced)
	}
	if docs := wire.in(folder)["summary.md"]; len(docs) != 1 {
		t.Errorf("folder holds %d summary.md, want 1", len(docs))
	}
}
