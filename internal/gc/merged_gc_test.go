package gc

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestGCDeletesMergedSourcesAndTheirRawRecordings(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for i, id := range []string{"source-a", "source-b"} {
		at := started.Add(time.Duration(i) * time.Hour)
		endWithRecording(t, store, dir, id, at, 1024)
	}
	for _, id := range []string{"source-a", "source-b"} {
		syncTo(t, store, id, storage.SyncStateSynced, "folder-"+id, 0, "")
	}
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	held := map[string]*os.File{}
	for _, id := range []string{"source-a", "source-b"} {
		raw := filepath.Join(dir, id+".pcm")
		contents := []byte("raw recording for " + id)
		if err := os.WriteFile(raw, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(raw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		held[id] = f
	}

	deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1)
	if len(deleted) != 1 || deleted[0] != "merged" {
		t.Fatalf("GC deleted %v, want [merged]", deleted)
	}
	for id, f := range held {
		if _, err := store.GetSession(id); err == nil {
			t.Errorf("merged source %s remains after GC", id)
		}
		raw := filepath.Join(dir, id+".pcm")
		if _, err := os.Stat(raw); !os.IsNotExist(err) {
			t.Errorf("raw recording %s after GC = %v, want removed", id, err)
		}
		got := make([]byte, len("raw recording for "+id))
		if _, err := f.ReadAt(got, 0); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, make([]byte, len(got))) {
			t.Errorf("raw recording %s after GC = %q, want zeroed", id, got)
		}
	}
}

func TestGCRefusesMergedSessionWithAnUnsafeSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*storage.SQLiteStore) error
	}{
		{
			name: "active",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`UPDATE sessions SET status = ? WHERE id = 'source-a'`, storage.SessionActive)
				return err
			},
		},
		{
			name: "replacement pending",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = 'source-a'`)
				return err
			},
		},
		{
			name: "folder owed",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`INSERT INTO drive_replacement_folders(session_id, generation, folder_session_id, state)
					VALUES('source-a', 1, 'source-a', ?)`, storage.DriveReplacementFolderOwed)
				return err
			},
		},
		{
			name: "remote deleted",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`UPDATE sessions SET sync_state = ?, sync_status = ? WHERE id = 'source-a'`, storage.SyncStateRemoteDeleted, storage.SyncFailed)
				return err
			},
		},
		{
			name: "unsynced",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`UPDATE sessions SET sync_state = ?, sync_status = ? WHERE id = 'source-a'`, storage.SyncStatePending, storage.SyncPending)
				return err
			},
		},
		{
			name: "documents unverified",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`UPDATE sessions SET documents_unverified = 1 WHERE id = 'source-a'`)
				return err
			},
		},
		{
			name: "refinement running",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ? WHERE id = 'source-a'`, storage.RefinementRunning)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := newStore(t)
			started := time.Now().UTC().Add(-40 * 24 * time.Hour)
			for i, id := range []string{"source-a", "source-b"} {
				endWithRecording(t, store, dir, id, started.Add(time.Duration(i)*time.Hour), 1024)
			}
			for _, id := range []string{"source-a", "source-b"} {
				syncTo(t, store, id, storage.SyncStateSynced, "folder-"+id, 0, "")
			}
			if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
				t.Fatal(err)
			}
			if err := tc.set(store); err != nil {
				t.Fatal(err)
			}

			if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 0 {
				t.Fatalf("GC deleted %v with an unsafe source, want none", deleted)
			}
			for _, id := range []string{"source-a", "source-b", "merged"} {
				if _, err := store.GetSession(id); err != nil {
					t.Errorf("GC removed %s despite unsafe source: %v", id, err)
				}
			}
		})
	}
}

func TestGCKeepsMergedSessionWhenASourceRawIsItsOnlyCopy(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	endWithRecording(t, store, dir, "source-a", started, 1024)
	if err := store.CreateSession("source-b", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("source-b", started.Add(time.Hour+time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source-a", "source-b"} {
		syncTo(t, store, id, storage.SyncStateSynced, "folder-"+id, 0, "")
	}
	raw := filepath.Join(dir, "source-b.pcm")
	if err := os.WriteFile(raw, []byte("the source's only audio copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Fatalf("GC deleted %v though source-b has only raw audio, want none", deleted)
	}
	if _, err := store.GetSession("merged"); err != nil {
		t.Fatalf("merged session after GC = %v, want kept", err)
	}
	if _, err := os.Stat(raw); err != nil {
		t.Fatalf("source-b raw after GC = %v, want kept", err)
	}
}

func TestGCZeroesReservedRawWhenDeletingASession(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	endWithRecording(t, store, dir, "session", started, 1024)
	if err := store.UpdateSummaryAtGeneration("session", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(dir, "session.pcm")
	contents := []byte("raw recording hidden before GC deletion")
	if err := os.WriteFile(raw, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	reservation, err := erase.Reserve(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveRawSessionErase("session", reservation.OriginalPath(), reservation.HiddenPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.Begin(); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(reservation.HiddenPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 1 {
		t.Fatalf("GC deleted %v, want [session]", deleted)
	}
	if _, err := os.Stat(reservation.HiddenPath()); !os.IsNotExist(err) {
		t.Fatalf("hidden raw after GC = %v, want removed", err)
	}
	got := make([]byte, len(contents))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatalf("hidden raw after GC = %q, want zeroed", got)
	}
}

func TestGCKeepsMergedSessionWhenASourceSavedRecordingVanishes(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for i, id := range []string{"source-a", "source-b"} {
		endWithRecording(t, store, dir, id, started.Add(time.Duration(i)*time.Hour), 1024)
		syncTo(t, store, id, storage.SyncStateSynced, "folder-"+id, 0, "")
	}
	raw := filepath.Join(dir, "source-b.pcm")
	if err := os.WriteFile(raw, []byte("source-b only audio after its saved recording vanishes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "source-b.mp3")); err != nil {
		t.Fatal(err)
	}

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Fatalf("GC deleted %v after source-b saved recording vanished, want none", deleted)
	}
	if _, err := os.Stat(raw); err != nil {
		t.Fatalf("source-b raw after GC = %v, want kept", err)
	}
}

func TestGCChecksEachMergedSourceDriveFolder(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for i, id := range []string{"source-a", "source-b"} {
		endWithRecording(t, store, dir, id, started.Add(time.Duration(i)*time.Hour), 1024)
		syncTo(t, store, id, storage.SyncStateSynced, "source-folder-"+id, 0, "")
	}
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	drive := &driveFolders{}
	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir, Drive: drive}, 1); len(deleted) != 1 || deleted[0] != "merged" {
		t.Fatalf("GC deleted %v, want [merged]", deleted)
	}
	asked := make(map[string]bool, len(drive.asked))
	for _, folderID := range drive.asked {
		asked[folderID] = true
	}
	for _, folderID := range []string{"source-folder-source-a", "source-folder-source-b"} {
		if !asked[folderID] {
			t.Errorf("Drive checks = %v, want %s", drive.asked, folderID)
		}
	}
}

func TestGCAllowsFolderlessUnsyncedMergedSource(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	endWithRecording(t, store, dir, "source-a", started, 1024)
	syncTo(t, store, "source-a", storage.SyncStateSynced, "source-folder-a", 0, "")
	endWithRecording(t, store, dir, "source-b", started.Add(time.Hour), 1024)
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 1 || deleted[0] != "merged" {
		t.Fatalf("GC deleted %v with a folderless unsynced source, want [merged]", deleted)
	}
}
