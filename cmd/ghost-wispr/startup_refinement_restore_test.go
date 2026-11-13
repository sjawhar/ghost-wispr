package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestRefineUnfinishedDeletionRestoresPartialAudio(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "startup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dir := t.TempDir()
	remaining := filepath.Join(dir, "remaining.mp3")
	removed := filepath.Join(dir, "removed.mp3")
	if err := os.WriteFile(remaining, []byte("remaining"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Minute), remaining); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_removed_at = ?, refinement_status = ?, gdrive_folder_id = ?, sync_status = ?, sync_state = ? WHERE id = ?`, "2026-11-13T10:00:00Z", storage.RefinementPending, "folder", storage.SyncSynced, storage.SyncStateSynced, "session"); err != nil {
		t.Fatal(err)
	}
	drive := &deletionDriveFake{
		files: []*drive.File{
			{Name: recordingname.FileName(removed), MimeType: "audio/mpeg"},
			{Name: recordingname.FileName(remaining), MimeType: "audio/mpeg"},
		},
		downloads: map[string][]byte{recordingname.FileName(removed): []byte("removed")},
	}
	outcome := refineUnfinishedDeletion(context.Background(), store, dir, drive, func(_ context.Context, id, audioPath string, gotStarted time.Time) error {
		if id != "session" || gotStarted != started || !strings.Contains(audioPath, removed) || !strings.Contains(audioPath, remaining) {
			t.Fatalf("refine session=%q audio=%q started=%s", id, audioPath, gotStarted)
		}
		return nil
	}, "session")
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
}

func TestRefineUnfinishedDeletionKeepsMarkerWhenDriveLacksRemovedAudio(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "startup-missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dir := t.TempDir()
	remaining := filepath.Join(dir, "remaining.mp3")
	if err := os.WriteFile(remaining, []byte("remaining"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Minute), remaining); err != nil {
		t.Fatal(err)
	}
	const removedAt = "2026-11-13T10:00:00Z"
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_removed_at = ?, refinement_status = ?, gdrive_folder_id = ?, sync_status = ?, sync_state = ? WHERE id = ?`, removedAt, storage.RefinementPending, "folder", storage.SyncSynced, storage.SyncStateSynced, "session"); err != nil {
		t.Fatal(err)
	}
	drive := &deletionDriveFake{files: []*drive.File{{Name: recordingname.FileName(remaining), MimeType: "audio/mpeg"}}}
	outcome := refineUnfinishedDeletion(context.Background(), store, dir, drive, func(context.Context, string, string, time.Time) error {
		t.Fatal("refined from survivors after Drive omitted a removed recording")
		return nil
	}, "session")
	if outcome.Err == nil {
		t.Fatal("startup refinement succeeded without restoring every recording")
	}
	session, err := store.GetSession("session")
	if err != nil {
		t.Fatal(err)
	}
	if session.AudioRemovedAt != removedAt {
		t.Fatalf("startup refinement cleared audio removal marker %q", session.AudioRemovedAt)
	}
}
