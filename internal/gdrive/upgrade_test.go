package gdrive

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// testdata/main-782dccb.db is a database the version before this change
// (782dccb) built with its own store and orchestrator, the Drive upload
// faked. Every session in it started on 2026-09-01, so the age policy finds
// it old (the restored one counts from its restore, on 2026-11-06):
//   - 20260901100000 was synced by the sweep while its summary was running;
//     its refined transcript and its summary ("Budget Review", "The summary
//     only the device has.") landed after, and never reached Drive. It is
//     synced, in folder-a.
//   - 20260901110000 was summarized, then synced: Drive has its documents.
//     It is synced, in folder-c.
//   - 20260901120000 was restored from Drive (synced, no sync state, in
//     folder-restored), then summarized again: Drive lacks the new summary.
//   - 20260901130000 was summarized and never synced.
const mainEraDB = "testdata/main-782dccb.db"

var mainEraSynced = []string{"20260901100000", "20260901110000", "20260901120000"}

// openCopy opens a copy of database file src, in a temporary directory, with
// this version's store, and returns the store and the copy's path.
func openCopy(t *testing.T, src string) (*storage.SQLiteStore, string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func ageGC(store *storage.SQLiteStore, syncGated bool) *gc.Collector {
	return gc.New(store, gc.Config{MaxAgeDays: 30, SyncGated: syncGated}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Review of #38, round 1, blocker 1: a session the version before left
// synced after its documents changed (synced while its summary was running,
// summarized again, its refinement retried, its title edited) held text Drive
// never had, and the age policy deleted it after the upgrade. Nothing records
// what an earlier version uploaded, so the upgrade marks every synced session
// as having documents not verified on Drive, and no GC deletes such a session
// until a sync of it completes.
func TestTheAgePolicyKeepsSessionsSyncedBeforeTheUpgrade(t *testing.T) {
	store, _ := openCopy(t, mainEraDB)

	deleted, err := ageGC(store, true).Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if len(deleted) != 0 {
		sess, _ := store.GetSession("20260901100000")
		t.Errorf("gated age GC deleted %v; 20260901100000's summary %q was never on Drive", deleted, sess.Summary)
	}
	for _, id := range mainEraSynced {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatalf("%s after GC: %v", id, err)
		}
		if sess.SyncStatus != storage.SyncSynced {
			t.Errorf("%s after the upgrade: sync status %q, want synced as before", id, sess.SyncStatus)
		}
	}
	if sess, _ := store.GetSession("20260901100000"); sess.Summary != "The summary only the device has." || !strings.Contains(sess.CanonicalTranscript, "only the device has") {
		t.Errorf("20260901100000 after GC: summary %q, transcript %q; want the text only the device has", sess.Summary, sess.CanonicalTranscript)
	}

	// With sync off, the age policy deletes what was never synced, as
	// before, and still none of the sessions it cannot know Drive has.
	if deleted, err = ageGC(store, false).Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if !slices.Equal(deleted, []string{"20260901130000"}) {
		t.Errorf("ungated age GC deleted %v, want only the never-synced 20260901130000", deleted)
	}
}

// A session synced before the upgrade whose documents change after it goes
// back to sync; once that sync completes, Drive has its documents, and the
// age policy deletes it, after a restart too. So does the session first
// synced since the upgrade; the sessions not synced since it stay.
func TestASessionSyncedAfterTheUpgradeIsVerified(t *testing.T) {
	store, path := openCopy(t, mainEraDB)
	const id = "20260901100000"
	if err := store.UpdateTitle(id, "Budget Review, final"); err != nil {
		t.Fatalf("edit title: %v", err)
	}
	drive := newFakeDrive()
	sweep(t, store, NewOrchestratorWithUploader(drive, store))
	if state, folder := sessionState(t, store, id); state != storage.SyncStateSynced || folder != "folder-a" {
		t.Fatalf("after the sweep: %q in folder %q, want synced in folder-a", state, folder)
	}
	_, summary := document(t, drive, "folder-a", "summary.md")
	_, transcript := document(t, drive, "folder-a", "transcript.md")
	if !strings.Contains(summary, "The summary only the device has.") || !strings.Contains(transcript, "The refined transcript only the device has.") {
		t.Fatalf("Drive after the sweep: summary.md\n%s\ntranscript.md\n%s\nwant the text only the device had", summary, transcript)
	}

	// The app restarts: the upgrade does not run again.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	deleted, err := ageGC(store, true).Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if !slices.Equal(deleted, []string{id, "20260901130000"}) {
		t.Errorf("gated age GC deleted %v, want only %s and 20260901130000, synced since the upgrade", deleted, id)
	}
}
