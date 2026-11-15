package gdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// wireRoots are the two folders the app can sync into: a folder in My Drive
// that files.get reads (under the drive.file scope, one the app created), and
// a shared drive's root, which under that scope files.get answers 404 for
// (wireDrive.sharedDrive).
var wireRoots = []struct {
	name    string
	newWire func(*testing.T) (*wireDrive, *Syncer)
}{
	{"My Drive folder", newWireDrive},
	{"shared drive", newSharedDriveWire},
}

// The wire Drive answers for a shared drive's root as Drive answered the app,
// through its drive.file scope and as its service account, on the office Pi
// on 2026-11-11, for the real drive's id and for a made-up one:
//  1. files.get: 404 "File not found" for both;
//  2. a list of the root's children without corpora: 200 with no files for
//     both, so it cannot tell them apart;
//  3. the same list with corpora "drive" and the id as driveId: 200 for the
//     drive, 404 "Shared drive not found" for the made-up id.
//
// A drive the app is no longer a member of answers as the made-up id; that
// was not measured (only Sami can remove the app from the drive).
func TestTheWireDriveAnswersASharedDriveRootAsDriveFileDoes(t *testing.T) {
	wire, syncer := newSharedDriveWire(t)
	ctx := context.Background()
	files := syncer.service.Files
	list := func(id string) *drive.FilesListCall {
		return files.List().Q(fmt.Sprintf("'%s' in parents and trashed = false", id)).
			SupportsAllDrives(true).IncludeItemsFromAllDrives(true)
	}
	answers := func(t *testing.T, id string, member bool) {
		t.Helper()
		if _, err := files.Get(id).SupportsAllDrives(true).Context(ctx).Do(); !IsRemoteDeletedError(err) {
			t.Errorf("files.get(%s): %v, want 404", id, err)
		}
		if got, err := list(id).Context(ctx).Do(); err != nil || len(got.Files) != 0 {
			t.Errorf("list of %s's children: %v, %v; want 200 with no files", id, got, err)
		}
		_, err := list(id).Corpora("drive").DriveId(id).Context(ctx).Do()
		switch {
		case member && err != nil:
			t.Errorf("list of shared drive %s: %v, want 200", id, err)
		case !member && (!IsRemoteDeletedError(err) || !strings.Contains(err.Error(), "Shared drive not found")):
			t.Errorf("list of shared drive %s: %v, want 404 Shared drive not found", id, err)
		}
	}

	answers(t, sharedDriveID, true)
	answers(t, "shared-drive-made-up", false)
	wire.deleteByHand(sharedDriveID)
	answers(t, sharedDriveID, false)
}

// gcRootPolicies are GC's two policies, each of which asks Drive before it
// deletes a session or removes its audio.
var gcRootPolicies = []struct {
	name   string
	config gc.Config
}{
	{"age policy", gc.Config{MaxAgeDays: 30}},
	{"audio budget", gc.Config{MaxAudioSizeMB: 1}},
}

// syncedOldSessions makes, and syncs, sessions ids, each older than GC's age
// limit, in order of age, the first the oldest, and each with a recording
// larger than its audio budget, and returns their recordings.
func syncedOldSessions(t *testing.T, store *storage.SQLiteStore, o *Orchestrator, audioDir string, ids ...string) map[string]string {
	t.Helper()
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	recordings := map[string]string{}
	for i, id := range ids {
		summarizedSession(t, store, audioDir, id, old.Add(time.Duration(i)*24*time.Hour), "The summary.")
		recordings[id] = filepath.Join(audioDir, id+".mp3")
		if err := os.WriteFile(recordings[id], make([]byte, 2*1024*1024), 0o644); err != nil {
			t.Fatal(err)
		}
		syncedSession(t, store, o, id)
	}
	return recordings
}

// keptWhole fails the test unless session id is still in the store, in sync
// state state, with its recording on disk and named.
func keptWhole(t *testing.T, store *storage.SQLiteStore, id, recording, state string) {
	t.Helper()
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("session %s deleted: %v", id, err)
	}
	if sess.SyncState != state {
		t.Errorf("%s after GC: %q, want %q", id, sess.SyncState, state)
	}
	if sess.AudioPath != recording || sess.AudioRemovedAt != "" {
		t.Errorf("%s: audio path %q, removed at %q: GC removed the recording", id, sess.AudioPath, sess.AudioRemovedAt)
	}
	if _, err := os.Stat(recording); err != nil {
		t.Errorf("%s's recording: %v", id, err)
	}
}

// A session folder deleted by hand from a root Drive still answers for is the
// session deleted on Drive: GC keeps the session, marks it REMOTE_DELETED,
// and goes on to the next one, which Drive has. Under the drive.file scope
// files.get answers 404 for a shared drive's root, so GC took every such 404
// for the app's access to Drive lost: it held the whole pass, for every
// session, until someone put the folder back, and no audio was freed.
func TestGCMarksAFolderDeletedFromTheRootAndCarriesOn(t *testing.T) {
	for _, root := range wireRoots {
		for _, policy := range gcRootPolicies {
			t.Run(root.name+" and "+policy.name, func(t *testing.T) {
				wire, syncer := root.newWire(t)
				store := openStore(t)
				o := NewOrchestrator(syncer, store)
				audioDir := t.TempDir()
				const gone, onDrive = "20260901100000", "20260902100000"
				recordings := syncedOldSessions(t, store, o, audioDir, gone, onDrive)
				_, folder := sessionState(t, store, gone)
				wire.deleteByHand(folder)

				config := policy.config
				config.AudioDir, config.Drive = audioDir, o
				collector := gc.New(store, config, slog.New(slog.NewTextHandler(io.Discard, nil)))
				deleted, err := collector.Run()
				if err != nil {
					t.Fatalf("gc run: %v", err)
				}
				if held := collector.Held(); held != nil {
					t.Errorf("GC held the pass: %v; want the session marked and the pass carried on", held)
				}
				keptWhole(t, store, gone, recordings[gone], storage.SyncStateRemoteDeleted)
				if policy.config.MaxAgeDays > 0 {
					if len(deleted) != 1 || deleted[0] != onDrive {
						t.Errorf("GC deleted %v, want [%s], the session Drive has", deleted, onDrive)
					}
					return
				}
				if _, err := os.Stat(recordings[onDrive]); !os.IsNotExist(err) {
					t.Errorf("GC kept the recording of %s, a session Drive has: %v", onDrive, err)
				}
			})
		}
	}
}

// A root Drive no longer answers for (a shared drive the app is no longer a
// member of, or one deleted; a folder in My Drive unshared or gone) makes
// every session folder in it answer 404 too, which says nothing about the
// sessions: GC holds the whole pass, with the app's access to Drive lost as
// its reason, deletes nothing, removes no audio and marks no session
// REMOTE_DELETED.
func TestGCHoldsWhenTheRootDoesNotRead(t *testing.T) {
	for _, root := range wireRoots {
		for _, policy := range gcRootPolicies {
			t.Run(root.name+" and "+policy.name, func(t *testing.T) {
				wire, syncer := root.newWire(t)
				store := openStore(t)
				o := NewOrchestrator(syncer, store)
				audioDir := t.TempDir()
				ids := []string{"20260901100000", "20260902100000"}
				recordings := syncedOldSessions(t, store, o, audioDir, ids...)
				wire.deleteByHand(wire.rootID) // and every session folder in it

				config := policy.config
				config.AudioDir, config.Drive = audioDir, o
				collector := gc.New(store, config, slog.New(slog.NewTextHandler(io.Discard, nil)))
				deleted, err := collector.Run()
				if err != nil {
					t.Fatalf("gc run: %v", err)
				}
				if len(deleted) != 0 {
					t.Errorf("GC deleted %v", deleted)
				}
				held := collector.Held()
				if !errors.Is(held, gc.ErrDriveCannotSay) || !strings.Contains(held.Error(), "access to Drive may be lost") {
					t.Errorf("Held() = %v, want %v with access to Drive lost", held, gc.ErrDriveCannotSay)
				}
				for _, id := range ids {
					keptWhole(t, store, id, recordings[id], storage.SyncStateSynced)
				}
			})
		}
	}
}

// Drive lists no folders, and no error, for an id it does not have, and so,
// as taken here (not measured), for a shared drive the app is not a member
// of. So a restore from such a root fails instead of reporting an empty
// Drive, as a restore from a folder in My Drive that Drive does not have
// fails. A root that reads and holds no session folder restores none, with
// no error.
func TestRestoreFromARootThatDoesNotReadFails(t *testing.T) {
	for _, root := range wireRoots {
		t.Run(root.name, func(t *testing.T) {
			wire, syncer := root.newWire(t)
			importNone := func(RestoredSession) error { return nil }

			result, err := syncer.RestoreFromDrive(context.Background(), importNone)
			if err != nil || result.Restored != 0 || len(result.Errors) != 0 {
				t.Fatalf("restore from an empty root: %+v, %v; want none restored and no error", result, err)
			}
			wire.deleteByHand(wire.rootID)
			result, err = syncer.RestoreFromDrive(context.Background(), importNone)
			if err == nil {
				t.Errorf("restore from a root Drive does not answer for: %+v, no error; want one", result)
			}
		})
	}
}
