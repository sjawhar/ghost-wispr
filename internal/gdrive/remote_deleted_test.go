package gdrive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// A session's Drive folder deleted by hand for good, from a folder in My
// Drive or from a shared drive's root: Drive answers 404 for it, through the
// real client. A sync into it ends REMOTE_DELETED, which no sync starts from
// (a new folder would put back what the user deleted), a retry by hand is
// refused for the folder gone (409), not for Drive not answering (503), and
// the session keeps its local copy.
func TestASyncIntoAFolderDeletedOnDriveEndsRemoteDeleted(t *testing.T) {
	for _, root := range wireRoots {
		t.Run(root.name, func(t *testing.T) {
			wire, syncer := root.newWire(t)
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
			if err := o.CanSync(context.Background(), id); err == nil || !strings.Contains(err.Error(), "still in the trash or gone") {
				t.Errorf("CanSync with the folder gone: %v, want why no retry can start", err)
			}
			// A retry asks Drive again, finds the folder still gone, and makes none.
			_ = o.SyncSession(context.Background(), id)
			if state, _ := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted {
				t.Errorf("after a retry: %q, want %q", state, storage.SyncStateRemoteDeleted)
			}
			if folders := wire.in(wire.rootID); len(folders) != 0 {
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
		})
	}
}

// A 404 for the Drive folder the app syncs into (a folder in My Drive
// unshared or mistyped, a shared drive the app is no longer a member of) is
// not the session's folder deleted by hand: the first sync of a session
// retries, as for any failure, and a retry by hand is possible once the root
// is back.
func TestASessionsFirstSyncIntoAMissingRootIsRetried(t *testing.T) {
	for _, root := range wireRoots {
		t.Run(root.name, func(t *testing.T) {
			wire, syncer := root.newWire(t)
			store := openStore(t)
			o := NewOrchestrator(syncer, store)
			const id = "20261106100000"
			summarizedSession(t, store, t.TempDir(), id, time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), "The summary.")

			wire.deleteByHand(wire.rootID)
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
		})
	}
}

// A root Drive no longer answers for makes every session folder in it answer
// 404, which says nothing about the sessions: a re-sync of a synced session
// retries with its folder kept, never REMOTE_DELETED, and a retry by hand of
// a session already REMOTE_DELETED is refused as Drive not answering (503),
// not as its folder gone (409).
func TestASyncIntoARootThatDoesNotReadIsRetried(t *testing.T) {
	for _, root := range wireRoots {
		t.Run(root.name, func(t *testing.T) {
			wire, syncer := root.newWire(t)
			store := openStore(t)
			o := NewOrchestrator(syncer, store)
			audioDir := t.TempDir()
			const resynced, deleted = "20261106090000", "20261106100000"
			summarizedSession(t, store, audioDir, resynced, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC), "The summary.")
			summarizedSession(t, store, audioDir, deleted, time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), "The summary.")
			folder := syncedSession(t, store, o, resynced)
			gone := syncedSession(t, store, o, deleted)
			// deleted's folder goes while the root reads: REMOTE_DELETED.
			wire.deleteByHand(gone)
			if err := store.UpdateTitle(deleted, "Renamed"); err != nil {
				t.Fatalf("rename: %v", err)
			}
			_ = o.SyncSession(context.Background(), deleted)
			if state, _ := sessionState(t, store, deleted); state != storage.SyncStateRemoteDeleted {
				t.Fatalf("%s with its folder gone: %q, want %q", deleted, state, storage.SyncStateRemoteDeleted)
			}

			wire.deleteByHand(wire.rootID)
			if err := store.UpdateTitle(resynced, "Renamed"); err != nil {
				t.Fatalf("rename: %v", err)
			}
			if err := o.SyncSession(context.Background(), resynced); err == nil || !strings.Contains(err.Error(), "access to Drive may be lost") {
				t.Errorf("re-sync into a root Drive does not answer for: %v, want access to Drive lost", err)
			}
			if state, kept := sessionState(t, store, resynced); state != storage.SyncStateRetryScheduled || kept != folder {
				t.Errorf("after the re-sync: %q in folder %q, want %q in %q", state, kept, storage.SyncStateRetryScheduled, folder)
			}
			if err := o.CanSync(context.Background(), deleted); !errors.Is(err, ErrDriveDidNotAnswer) {
				t.Errorf("CanSync of %s with the root gone: %v, want %v", deleted, err, ErrDriveDidNotAnswer)
			}
		})
	}
}
