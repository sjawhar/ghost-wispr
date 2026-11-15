package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestRestoreRefinementRefusesWhileDriveReplacementIsOwed(t *testing.T) {
	for name, hold := range map[string]func(*testing.T, *storage.SQLiteStore){
		"session replacement pending": func(t *testing.T, store *storage.SQLiteStore) {
			if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = ?`, "session"); err != nil {
				t.Fatal(err)
			}
		},
		"folder replacement owed": func(t *testing.T, store *storage.SQLiteStore) {
			if _, err := store.DB().Exec(`INSERT INTO drive_replacement_folders(session_id, generation, folder_session_id, state) VALUES(?, ?, ?, ?)`, "session", 0, "session", storage.DriveReplacementFolderOwed); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "consent.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
			if err := store.CreateSession("session", started); err != nil {
				t.Fatal(err)
			}
			if err := store.EndSession("session", started.Add(time.Minute), ""); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`UPDATE sessions SET audio_removed_at = ?, gdrive_folder_id = ?, sync_status = ?, sync_state = ? WHERE id = ?`, "2026-11-13T10:00:00Z", "folder", storage.SyncSynced, storage.SyncStateSynced, "session"); err != nil {
				t.Fatal(err)
			}
			hold(t, store)
			session, err := store.GetSession("session")
			if err != nil {
				t.Fatal(err)
			}
			err = restoreRefinementRecording(context.Background(), store, &session, t.TempDir(), func(context.Context, storage.Session) (deletionRecordingSet, error) {
				t.Fatal("attempted to restore a pre-replacement Drive recording")
				return deletionRecordingSet{}, nil
			})
			if err == nil {
				t.Fatal("restored audio while a Drive replacement was owed")
			}
		})
	}
}
