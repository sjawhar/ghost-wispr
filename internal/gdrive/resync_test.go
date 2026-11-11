package gdrive

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// openStore opens a store on a new database in a temporary directory.
func openStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// summarizedSession creates session id, started at started and ended with a
// recording in audioDir and a streamed transcript, with summary as its
// summary and its transcript canonicalized, as a session is once its summary
// is made.
func summarizedSession(t *testing.T, store *storage.SQLiteStore, audioDir, id string, started time.Time, summary string) {
	t.Helper()
	recording := filepath.Join(audioDir, id+".mp3")
	if err := os.WriteFile(recording, []byte("audio of "+id), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.AppendSegment(id, transcribe.Segment{Text: "The streamed transcript.", StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
		t.Fatalf("append segment: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if _, _, err := store.Canonicalize(id); err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if err := store.UpdateSummary(id, "Standup", summary, storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
}

// resummarize makes session id's summary again, as the app does: its summary
// is running, the last one kept, then the new one is stored.
func resummarize(t *testing.T, store *storage.SQLiteStore, id, summary string) {
	t.Helper()
	if err := store.StartSummary(id); err != nil {
		t.Fatalf("start summary: %v", err)
	}
	if err := store.UpdateSummary(id, "Standup", summary, storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("store summary: %v", err)
	}
}

// sweep syncs what the app's sync sweep syncs (sweepDriveSync in
// cmd/ghost-wispr): the sessions waiting for a sync, and those whose retry is
// due.
func sweep(t *testing.T, store *storage.SQLiteStore, o *Orchestrator) {
	t.Helper()
	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatalf("sessions needing sync: %v", err)
	}
	retries, err := store.GetSessionsBySyncState(storage.SyncStateRetryScheduled)
	if err != nil {
		t.Fatalf("sessions to retry: %v", err)
	}
	for i := range retries {
		if IsRetryReady(time.Now(), retries[i].RetryCount, retries[i].LastSyncAttempt) && !slices.Contains(ids, retries[i].ID) {
			ids = append(ids, retries[i].ID)
		}
	}
	for _, id := range ids {
		if err := o.SyncSession(context.Background(), id); err != nil {
			t.Errorf("sweep: sync %s: %v", id, err)
		}
	}
}

// syncedSession syncs session id and returns its Drive folder, failing the
// test unless it ends synced.
func syncedSession(t *testing.T, store *storage.SQLiteStore, o *Orchestrator, id string) string {
	t.Helper()
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("sync: %v", err)
	}
	state, folder := sessionState(t, store, id)
	if state != storage.SyncStateSynced || folder == "" {
		t.Fatalf("after the sync: %q in folder %q, want synced in a folder", state, folder)
	}
	return folder
}

// document returns the one copy of document name in Drive folder folder, and
// its id, failing the test if the folder holds another number of copies.
func document(t *testing.T, drive *fakeDrive, folder, name string) (id, content string) {
	t.Helper()
	drive.mu.Lock()
	defer drive.mu.Unlock()
	var copies []fakeDriveFile
	for _, f := range drive.folders[folder] {
		if f.Name == name {
			copies = append(copies, f)
		}
	}
	if len(copies) != 1 {
		t.Fatalf("folder %s holds %d %s, want 1", folder, len(copies), name)
	}
	return copies[0].ID, string(copies[0].content)
}

// Review of #37, round 5, note 1 (on main too): a summary or transcript that
// changed after its session was synced (a summary made again, a refinement
// retried, a title edited) never reached Drive: the session stayed synced, so
// no sync ran again, and the age policy then deleted the newest text. A change
// to a synced session's documents now sends it back to sync, keeping its
// folder, and the sweep updates the documents there in place.
func TestAChangeAfterASyncReachesDrive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(t *testing.T, store *storage.SQLiteStore, id string)
		document string
		want     string
	}{
		{
			name: "summary made again",
			change: func(t *testing.T, store *storage.SQLiteStore, id string) {
				resummarize(t, store, id, "The summary, made again.")
			},
			document: "summary.md",
			want:     "The summary, made again.",
		},
		{
			name: "title edited",
			change: func(t *testing.T, store *storage.SQLiteStore, id string) {
				if err := store.UpdateTitle(id, "Planning, renamed"); err != nil {
					t.Fatalf("edit title: %v", err)
				}
			},
			document: "summary.md",
			want:     "# Planning, renamed",
		},
		{
			name: "refinement retried",
			change: func(t *testing.T, store *storage.SQLiteStore, id string) {
				if err := store.UpdateRefinement(id, "The refined transcript.", storage.RefinementCompleted); err != nil {
					t.Fatalf("refine: %v", err)
				}
				if _, _, err := store.Canonicalize(id); err != nil {
					t.Fatalf("canonicalize: %v", err)
				}
			},
			document: "transcript.md",
			want:     "The refined transcript.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t)
			const id = "20261106100000"
			summarizedSession(t, store, t.TempDir(), id, time.Now().UTC().Add(-time.Hour), "The first summary.")
			drive := newFakeDrive()
			o := NewOrchestratorWithUploader(drive, store)
			folder := syncedSession(t, store, o, id)
			before, _ := document(t, drive, folder, tc.document)

			tc.change(t, store, id)
			sweep(t, store, o)

			if state, inFolder := sessionState(t, store, id); state != storage.SyncStateSynced || inFolder != folder {
				t.Errorf("after the sweep: %q in folder %q, want synced in %s", state, inFolder, folder)
			}
			if len(drive.folders) != 1 {
				t.Errorf("Drive holds %d folders, want the session's one", len(drive.folders))
			}
			after, content := document(t, drive, folder, tc.document)
			if !strings.Contains(content, tc.want) {
				t.Errorf("Drive's %s after the sweep:\n%s\nwant it to hold %q", tc.document, content, tc.want)
			}
			if after != before {
				t.Errorf("%s is file %s after the sweep, want %s updated in place", tc.document, after, before)
			}
		})
	}
}

// A change to the documents during a sync (the summary made again while the
// upload runs) is not the sync failing: the session goes back to pending
// sync, keeping the folder the sync made and its retry count, and the next
// sweep uploads it as it is now. Counted as a failed attempt it waited out a
// retry's backoff, and five such changes left it FAILED until a manual retry.
func TestAChangeDuringASyncSendsTheSessionBackToPending(t *testing.T) {
	store := openStore(t)
	const id = "20261106100000"
	summarizedSession(t, store, t.TempDir(), id, time.Now().UTC().Add(-time.Hour), "The first summary.")
	drive := newFakeDrive()
	onFirstUpload(drive, func() { resummarize(t, store, id, "The summary, made again.") })
	o := NewOrchestratorWithUploader(drive, store)

	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Fatal("the sync whose session changed during the upload reported success")
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.SyncState != storage.SyncStatePending || sess.RetryCount != 0 || sess.GDriveFolderID == "" {
		t.Fatalf("after the sync: %q, retry %d, folder %q; want %q, retry 0, in the folder the sync made",
			sess.SyncState, sess.RetryCount, sess.GDriveFolderID, storage.SyncStatePending)
	}

	sweep(t, store, o)
	if state, folder := sessionState(t, store, id); state != storage.SyncStateSynced || folder != sess.GDriveFolderID {
		t.Errorf("after the sweep: %q in folder %q, want synced in %s", state, folder, sess.GDriveFolderID)
	}
	if _, summary := document(t, drive, sess.GDriveFolderID, "summary.md"); !strings.Contains(summary, "The summary, made again.") {
		t.Errorf("Drive's summary.md after the sweep:\n%s\nwant the summary made again", summary)
	}
}

// The age policy, gated on sync, deleted a synced session whose summary had
// changed since its sync: the newest summary existed only on the device. It
// now keeps the session until its current documents are on Drive.
func TestAgeGCKeepsASessionUntilItsCurrentDocumentsAreOnDrive(t *testing.T) {
	store := openStore(t)
	audioDir := t.TempDir()
	const id = "20260901100000"
	summarizedSession(t, store, audioDir, id, time.Now().UTC().AddDate(0, 0, -40), "The first summary.")
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)
	folder := syncedSession(t, store, o, id)
	collector := gc.New(store, gc.Config{MaxAgeDays: 30, SyncGated: true, AudioDir: audioDir, Drive: o}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	resummarize(t, store, id, "The summary, made again.")
	deleted, err := collector.Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if slices.Contains(deleted, id) {
		_, onDrive := document(t, drive, folder, "summary.md")
		t.Fatalf("GC deleted the session, and Drive's summary.md is:\n%s\nThe summary made again is lost.", onDrive)
	}

	sweep(t, store, o)
	if deleted, err = collector.Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if !slices.Contains(deleted, id) {
		t.Errorf("GC kept the session once it was synced again (deleted %v)", deleted)
	}
	if _, onDrive := document(t, drive, folder, "summary.md"); !strings.Contains(onDrive, "The summary, made again.") {
		t.Errorf("GC deleted the session with Drive's summary.md:\n%s\nwant the summary made again", onDrive)
	}
}

// changingStore is the real store with change run once, just after GC lists
// the sessions the age policy deletes and before it deletes the first.
type changingStore struct {
	*storage.SQLiteStore
	once   sync.Once
	change func()
}

func (s *changingStore) GetSession(id string) (storage.Session, error) {
	s.once.Do(s.change)
	return s.SQLiteStore.GetSession(id)
}

// A synced session whose summary is made again after GC listed it, and
// before GC deletes it, is kept: GC deletes a session only while it is synced.
func TestAgeGCKeepsASessionThatChangesAfterGCListsIt(t *testing.T) {
	store := openStore(t)
	audioDir := t.TempDir()
	const id = "20260901100000"
	summarizedSession(t, store, audioDir, id, time.Now().UTC().AddDate(0, 0, -40), "The first summary.")
	o := NewOrchestratorWithUploader(newFakeDrive(), store)
	syncedSession(t, store, o, id)

	changing := &changingStore{SQLiteStore: store, change: func() { resummarize(t, store, id, "The summary, made again.") }}
	deleted, err := gc.New(changing, gc.Config{MaxAgeDays: 30, SyncGated: true, AudioDir: audioDir, Drive: o}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if slices.Contains(deleted, id) {
		t.Errorf("GC deleted the session whose summary was made again after GC listed it; Drive never had that summary")
	}
	if _, err := store.GetSession(id); err != nil {
		t.Errorf("read session after GC: %v", err)
	}
}
