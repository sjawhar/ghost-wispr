package gdrive

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// A session folder moved to the Drive trash (deleting it in the Drive UI)
// still exists, so Drive does not answer 404 for it, and a list of what is
// not trashed finds nothing in it. A re-sync must not upload the session into
// it again and mark it synced: it ends REMOTE_DELETED, as for a folder
// deleted for good, and nothing in the folder changes.
func TestASyncNeverUploadsIntoAFolderInTheTrash(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	const id = "20261106090000"
	summarizedSession(t, store, t.TempDir(), id, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC), "The summary.")
	folder := syncedSession(t, store, o, id)
	before := wire.in(folder)

	wire.trash(folder)
	if err := store.UpdateTitle(id, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Error("sync into a folder in the trash: no error")
	}

	if state, kept := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted || kept != folder {
		t.Errorf("after the sync: %q in folder %q, want %q in %q", state, kept, storage.SyncStateRemoteDeleted, folder)
	}
	after := wire.in(folder)
	if len(after) != len(before) {
		t.Errorf("folder in the trash holds %v, held %v: the sync uploaded into it", keysOf(after), keysOf(before))
	}
	for _, doc := range after["summary.md"] {
		if strings.Contains(string(doc.Content), "Renamed") {
			t.Error("summary.md in the trash was updated")
		}
	}
}

// A session folder moved to the trash while a sync uploads into it: the
// sync does not mark the session synced.
func TestASyncNeverMarksSyncedAFolderTrashedDuringIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// resync: the session was synced before, and changes.
		resync bool
		want   string
	}{
		// The first sync made the folder: a 404 or the trash is retried
		// there, and the retry, into the folder the session now has, ends
		// REMOTE_DELETED.
		{"first sync", false, storage.SyncStateRetryScheduled},
		{"re-sync", true, storage.SyncStateRemoteDeleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, syncer := newWireDrive(t)
			store := openStore(t)
			o := NewOrchestrator(syncer, store)
			const id = "20261106100000"
			summarizedSession(t, store, t.TempDir(), id, time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), "The summary.")
			if tc.resync {
				syncedSession(t, store, o, id)
				if err := store.UpdateTitle(id, "Renamed"); err != nil {
					t.Fatalf("rename: %v", err)
				}
			}
			wire.onUpload = func(folder string) { wire.trashLocked(folder) }

			if err := o.SyncSession(context.Background(), id); err == nil {
				t.Error("sync into a folder trashed during it: no error")
			}
			if state, _ := sessionState(t, store, id); state != tc.want {
				t.Errorf("after the sync: %q, want %q", state, tc.want)
			}
			if !tc.resync {
				// The retry finds the folder in the trash.
				wire.onUpload = nil
				_ = o.SyncSession(context.Background(), id)
				if state, _ := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted {
					t.Errorf("after the retry: %q, want %q", state, storage.SyncStateRemoteDeleted)
				}
			}
		})
	}
}

// A synced session whose Drive folder is then moved to the trash, or deleted
// for good, has its only lasting copy on the device: Drive empties its trash
// after 30 days. GC, gated on sync, does not delete it for its age nor
// remove its recording to free space.
func TestGCKeepsASyncedSessionWhoseFolderIsInTheTrashOrGone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(wire *wireDrive, folder string)
		config gc.Config
	}{
		// No commas: a test's temporary directory is named after it, and an
		// audio path is split on commas.
		{"trash and age policy", (*wireDrive).trash, gc.Config{MaxAgeDays: 30}},
		{"trash and audio budget", (*wireDrive).trash, gc.Config{MaxAudioSizeMB: 1}},
		{"deleted for good and age policy", (*wireDrive).deleteByHand, gc.Config{MaxAgeDays: 30}},
		{"deleted for good and audio budget", (*wireDrive).deleteByHand, gc.Config{MaxAudioSizeMB: 1}},
		{"on Drive and age policy", nil, gc.Config{MaxAgeDays: 30}},
		{"on Drive and audio budget", nil, gc.Config{MaxAudioSizeMB: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, syncer := newWireDrive(t)
			store := openStore(t)
			o := NewOrchestrator(syncer, store)
			audioDir := t.TempDir()
			const id = "20260901100000"
			summarizedSession(t, store, audioDir, id, time.Now().UTC().Add(-60*24*time.Hour), "The summary.")
			recording := filepath.Join(audioDir, id+".mp3")
			if err := os.WriteFile(recording, make([]byte, 2*1024*1024), 0o644); err != nil {
				t.Fatal(err)
			}
			folder := syncedSession(t, store, o, id)

			if tc.remove == nil {
				// The guard: GC still deletes, and removes audio, when the
				// folder is on Drive.
				tc.config.AudioDir = audioDir
				tc.config.Drive = o
				deleted, err := gc.New(store, tc.config, slog.New(slog.NewTextHandler(io.Discard, nil))).Run()
				if err != nil {
					t.Fatalf("gc run: %v", err)
				}
				if _, err := os.Stat(recording); !os.IsNotExist(err) {
					t.Errorf("GC kept the recording of a session Drive has (deleted %v): %v", deleted, err)
				}
				return
			}
			tc.remove(wire, folder)
			tc.config.AudioDir = audioDir
			tc.config.Drive = o
			deleted, err := gc.New(store, tc.config, slog.New(slog.NewTextHandler(io.Discard, nil))).Run()
			if err != nil {
				t.Fatalf("gc run: %v", err)
			}

			if len(deleted) != 0 {
				t.Errorf("GC deleted %v", deleted)
			}
			sess, err := store.GetSession(id)
			if err != nil {
				t.Fatalf("session deleted: %v", err)
			}
			if sess.AudioPath != recording || sess.AudioRemovedAt != "" {
				t.Errorf("audio path %q, removed at %q: GC removed the recording", sess.AudioPath, sess.AudioRemovedAt)
			}
			if _, err := os.Stat(recording); err != nil {
				t.Errorf("recording: %v", err)
			}
			if sess.SyncState != storage.SyncStateRemoteDeleted || sess.GDriveFolderID != folder {
				t.Errorf("after GC: %q in folder %q, want %q in %q: no later pass or sync touches it", sess.SyncState, sess.GDriveFolderID, storage.SyncStateRemoteDeleted, folder)
			}
		})
	}
}

// The folder the app syncs into moved to the Drive trash puts every session
// folder in the trash with it, though none was deleted: restoring the root
// brings them all back. So no session is marked REMOTE_DELETED: a re-sync
// and a first sync retry (the first sync makes no folder in the trash), GC
// deletes nothing and holds the pass, and once the root is back every sync
// completes.
func TestATrashedRootMarksNoSessionDeleted(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	audioDir := t.TempDir()
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	const resynced, kept, first = "20260901100000", "20260902100000", "20261106100000"
	summarizedSession(t, store, audioDir, resynced, old, "The summary.")
	summarizedSession(t, store, audioDir, kept, old.Add(24*time.Hour), "The summary.")
	summarizedSession(t, store, audioDir, first, time.Now().UTC().Add(-time.Hour), "The summary.")
	syncedSession(t, store, o, resynced)
	syncedSession(t, store, o, kept)
	folders := len(wire.in("root"))

	wire.trash("root")
	collector := gc.New(store, gc.Config{MaxAgeDays: 30, AudioDir: audioDir, Drive: o}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if deleted, err := collector.Run(); err != nil || len(deleted) != 0 {
		t.Errorf("GC with the root in the trash: deleted %v (%v), want none", deleted, err)
	}
	if !errors.Is(collector.Held(), gc.ErrDriveCannotSay) {
		t.Errorf("Held() = %v, want %v", collector.Held(), gc.ErrDriveCannotSay)
	}
	if err := store.UpdateTitle(resynced, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	for _, id := range []string{resynced, first} {
		if err := o.SyncSession(context.Background(), id); err == nil {
			t.Errorf("sync %s with the root in the trash: no error", id)
		}
	}
	for id, want := range map[string]string{resynced: storage.SyncStateRetryScheduled, kept: storage.SyncStateSynced, first: storage.SyncStateRetryScheduled} {
		if state, _ := sessionState(t, store, id); state != want {
			t.Errorf("%s with the root in the trash: %q, want %q", id, state, want)
		}
	}
	if n := len(wire.in("root")); n != folders {
		t.Errorf("root holds %d folders, held %d: a first sync made a folder in the trash", n, folders)
	}

	wire.mu.Lock()
	delete(wire.trashed, "root")
	wire.mu.Unlock()
	for _, id := range []string{resynced, first} {
		if err := o.SyncSession(context.Background(), id); err != nil {
			t.Errorf("sync %s once the root is back: %v", id, err)
		}
		if state, _ := sessionState(t, store, id); state != storage.SyncStateSynced {
			t.Errorf("%s once the root is back: %q, want synced", id, state)
		}
	}
}

// A session marked REMOTE_DELETED keeps its folder. A retry asks Drive
// again: while the folder is still in the trash, CanSync says so (the API
// answers 409 with why), and a sync writes nothing and leaves the session
// REMOTE_DELETED; once the user restores the folder, the retry syncs the
// session into it, as it is now.
func TestARetryResumesASessionWhoseFolderIsBack(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	const id = "20261106090000"
	summarizedSession(t, store, t.TempDir(), id, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC), "The summary.")
	folder := syncedSession(t, store, o, id)
	wire.trash(folder)
	if err := store.UpdateTitle(id, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	_ = o.SyncSession(context.Background(), id)
	if state, _ := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted {
		t.Fatalf("after the sync into the trash: %q, want %q", state, storage.SyncStateRemoteDeleted)
	}
	before := len(wire.in(folder)["summary.md"])

	// Still in the trash: CanSync says so, and a sync writes nothing.
	if err := o.CanSync(context.Background(), id); err == nil || !strings.Contains(err.Error(), "still in the trash or gone") {
		t.Errorf("CanSync with the folder still in the trash: %v, want why no retry can start", err)
	}
	_ = o.SyncSession(context.Background(), id)
	if state, kept := sessionState(t, store, id); state != storage.SyncStateRemoteDeleted || kept != folder {
		t.Errorf("retry with the folder still in the trash: %q in %q, want %q in %q", state, kept, storage.SyncStateRemoteDeleted, folder)
	}
	// Drive not answering: CanSync says it cannot tell.
	wire.mu.Lock()
	wire.unavailable = true
	wire.mu.Unlock()
	if err := o.CanSync(context.Background(), id); !errors.Is(err, ErrDriveDidNotAnswer) {
		t.Errorf("CanSync with Drive not answering: %v, want %v", err, ErrDriveDidNotAnswer)
	}
	wire.mu.Lock()
	wire.unavailable = false
	wire.mu.Unlock()

	// Restored from the trash.
	wire.mu.Lock()
	delete(wire.trashed, folder)
	wire.mu.Unlock()
	if err := o.CanSync(context.Background(), id); err != nil {
		t.Errorf("CanSync with the folder back: %v", err)
	}
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Errorf("retry with the folder back: %v", err)
	}
	if state, kept := sessionState(t, store, id); state != storage.SyncStateSynced || kept != folder {
		t.Errorf("retry with the folder back: %q in %q, want %q in %q", state, kept, storage.SyncStateSynced, folder)
	}
	docs := wire.in(folder)["summary.md"]
	if len(docs) != before || !strings.Contains(string(docs[len(docs)-1].Content), "Renamed") {
		t.Errorf("summary.md: %d copies, want the %d there updated in place to the new title", len(docs), before)
	}
	if n := len(wire.in("root")); n != 1 {
		t.Errorf("root holds %d folders, want the one: no folder is made again", n)
	}
}
