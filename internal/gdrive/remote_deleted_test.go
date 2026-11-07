package gdrive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// The Syncer wraps the errors of the Drive client it returns ("list folder
// %s: %w"). A 404 is seen through the wrapping.
func TestIsRemoteDeletedErrorSeesThroughWrapping(t *testing.T) {
	notFound := &googleapi.Error{Code: 404, Message: "File not found: folder-1."}
	if !IsRemoteDeletedError(fmt.Errorf("list folder folder-1: %w", notFound)) {
		t.Error("a wrapped 404 is not seen as a remote deletion")
	}
	if IsRemoteDeletedError(fmt.Errorf("list folder folder-1: %w", &googleapi.Error{Code: 500})) {
		t.Error("a wrapped 500 is seen as a remote deletion")
	}
}

// A session's Drive folder deleted by hand for good: Drive answers 404 for
// it, through the real client. A sync into it ends REMOTE_DELETED, which no
// sync starts from (a new folder would put back what the user deleted), and
// the session keeps its local copy.
func TestASyncIntoAFolderDeletedOnDriveEndsRemoteDeleted(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	audioDir := t.TempDir()
	const id = "20261106090000"
	summarizedSession(t, store, audioDir, id, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC), "The summary.")
	folder := syncedSession(t, store, o, id)

	wire.deleteByHand(folder)
	// A change after the sync sends the session back to Drive, into its folder.
	if err := store.UpdateTitle(id, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Fatal("sync into a folder Drive does not have: no error")
	}

	if state, kept := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted || kept != folder {
		t.Errorf("after the sync: %q in folder %q, want %q in %q", state, kept, storage.SyncStateRemoteDeleted, folder)
	}
	// A retry asks Drive again, finds the folder still gone, and makes none.
	_ = o.SyncSession(context.Background(), id)
	if state, _ := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted {
		t.Errorf("after a retry: %q, want %q", state, storage.SyncStateRemoteDeleted)
	}
	if folders := wire.in("root"); len(folders) != 0 {
		t.Errorf("Drive root holds %v, want nothing: the deleted folder is not made again", keysOf(folders))
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Summary != "The summary." || sess.Title != "Renamed" {
		t.Errorf("local copy: title %q, summary %q; want it kept", sess.Title, sess.Summary)
	}
	if _, err := os.Stat(filepath.Join(audioDir, id+".mp3")); err != nil {
		t.Errorf("local recording: %v", err)
	}
}

// A 404 for the Drive folder the app syncs into (its root, unshared or
// mistyped) is not the session's folder deleted by hand: the first sync of a
// session retries, as for any failure, and a retry by hand is possible once
// the root is back.
func TestASessionsFirstSyncIntoAMissingRootIsRetried(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	const id = "20261106100000"
	summarizedSession(t, store, t.TempDir(), id, time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), "The summary.")

	wire.deleteByHand("root")
	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Fatal("sync into a root Drive does not have: no error")
	}

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.SyncState != storage.SyncStateRetryScheduled || sess.RetryCount != 1 {
		t.Errorf("after the sync: %q, retry %d; want %q, retry 1", sess.SyncState, sess.RetryCount, storage.SyncStateRetryScheduled)
	}
}
