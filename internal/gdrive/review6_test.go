package gdrive

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const sourceB = "20261106193000"

// TestATrashedSourceFolderDoesNotHoldBackTheMergedFolder fails if a merged
// source's folder in the Drive trash keeps the replacement from the folders
// Drive can reach (the merged session's own folder would keep the deleted
// words until the source is restored or purged, up to 30 days), or if the
// deletion finishes while the trashed folder is still owed.
func TestATrashedSourceFolderDoesNotHoldBackTheMergedFolder(t *testing.T) {
	store := openStore(t)
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	merged, folderA, recA := mergedDeletion(t, store, o, true)
	mergedSession, err := store.GetSession(merged)
	if err != nil {
		t.Fatal(err)
	}
	mergedFolder := mergedSession.GDriveFolderID
	if got := heldSecret(wire, mergedFolder, recA); len(got) == 0 {
		t.Fatal("control: the merged folder does not hold the deleted words before the replacement")
	}
	wire.allowDeletes = true
	wire.trash(folderA)

	if err := o.SyncSession(context.Background(), merged); err == nil {
		t.Fatal("the replacement succeeded with a source folder in the trash")
	}
	if got := heldSecret(wire, mergedFolder, recA); len(got) != 0 {
		t.Errorf("with a source folder in the trash, the merged folder still holds the deleted words in %v", got)
	}
	generation, err := store.DeletionGeneration(merged)
	if err != nil {
		t.Fatal(err)
	}
	folders, err := store.DriveReplacementFolders(merged, generation)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, folder := range folders {
		states[folder.FolderSessionID] = folder.State
	}
	if states[merged] != storage.DriveReplacementFolderReplaced || states[sourceB] != storage.DriveReplacementFolderReplaced || states["20261106190000"] != storage.DriveReplacementFolderOwed {
		t.Errorf("ledger after the first attempt = %v, want the merged and B folders replaced and the trashed A folder owed", states)
	}
	if sess, err := store.GetSession(merged); err != nil || !sess.DriveReplacePending {
		t.Fatalf("pending=%v (%v) with a folder still owed, want the deletion still owed", sess.DriveReplacePending, err)
	}

	wire.mu.Lock()
	delete(wire.trashed, folderA)
	wire.mu.Unlock()
	if err := o.SyncSession(context.Background(), merged); err != nil {
		t.Fatalf("replacement once the source folder was restored: %v", err)
	}
	if got := heldSecret(wire, folderA, recA); len(got) != 0 {
		t.Errorf("the restored source folder still holds the deleted words in %v", got)
	}
	if sess, err := store.GetSession(merged); err != nil || sess.DriveReplacePending {
		t.Errorf("pending=%v (%v) once every folder is replaced, want the deletion finished", sess.DriveReplacePending, err)
	}
}

// TestASourceFolderGetsOnlyTheRecordingsItHeld fails if the replacement
// uploads into a merged source's folder a spliced recording that folder never
// held (another source's).
func TestASourceFolderGetsOnlyTheRecordingsItHeld(t *testing.T) {
	store := openStore(t)
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	merged, _, recA := mergedDeletion(t, store, o, true)
	wire.allowDeletes = true

	if err := o.SyncSession(context.Background(), merged); err != nil {
		t.Fatalf("replacement: %v", err)
	}
	source, err := store.GetSession(sourceB)
	if err != nil {
		t.Fatal(err)
	}
	if copies := wire.in(source.GDriveFolderID)["audio-"+filepath.Base(recA)]; len(copies) != 0 {
		t.Errorf("source B's folder got %d copies of source A's spliced recording, which it never held", len(copies))
	}
}

// TestASourceOnlyReplacementKeepsTheStateItClaimedFrom fails if a replacement
// that did not touch the session's own Drive folder (it has none, or an
// earlier attempt found it gone) releases the session to PENDING rather than
// the state it claimed it from: a REMOTE_DELETED session would be synced
// into a folder that is gone, and a scheduled retry would lose its backoff.
func TestASourceOnlyReplacementKeepsTheStateItClaimedFrom(t *testing.T) {
	for _, tc := range []struct {
		name       string
		syncMerged bool
		state      string
	}{
		{name: "no folder of its own, retry scheduled", state: storage.SyncStateRetryScheduled},
		{name: "own folder found gone earlier", syncMerged: true, state: storage.SyncStateRemoteDeleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t)
			wire, syncer := newWireDrive(t)
			o := NewOrchestrator(syncer, store)
			merged, folderA, recA := mergedDeletion(t, store, o, tc.syncMerged)
			if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = ?, retry_count = 2 WHERE id = ?`, tc.state, merged); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`UPDATE drive_replacement_folders SET state = ? WHERE session_id = ? AND folder_session_id = ?`,
				storage.DriveReplacementFolderGone, merged, merged); err != nil {
				t.Fatal(err)
			}
			wire.allowDeletes = true

			if err := o.SyncSession(context.Background(), merged); err != nil {
				t.Fatalf("source-only replacement: %v", err)
			}
			if got := heldSecret(wire, folderA, recA); len(got) != 0 {
				t.Fatalf("the source folder still holds the deleted words in %v", got)
			}
			sess, err := store.GetSession(merged)
			if err != nil {
				t.Fatal(err)
			}
			if sess.SyncState != tc.state || sess.RetryCount != 2 {
				t.Errorf("after a source-only replacement: state=%q retry=%d, want the claimed-from %q with retry 2", sess.SyncState, sess.RetryCount, tc.state)
			}
		})
	}
}
