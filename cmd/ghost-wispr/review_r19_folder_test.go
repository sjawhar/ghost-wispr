package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// A and B are merged into m1 before either syncs, so neither has a folder.
// m2 = merge(m1, C) syncs to folder-m2, which holds A, B and C, and then GC
// removes m2's audio. m3 = merge(m2, D). Retrying m3 must restore A and B
// from folder-m2, two merges above them.
func TestReviewR19FolderTwoLevelsUp(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	a, b, c, d := "20261113090000", "20261113100000", "20261113110000", "20261113120000"
	path := func(id string) string { return filepath.Join(dir, id+".mp3") }
	for i, id := range []string{a, b, c, d} {
		if err := store.CreateSession(id, start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, start.Add(time.Duration(i)*time.Hour+time.Minute), path(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path(d), []byte("d"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m1", []string{a, b}, start, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m2", []string{"m1", c}, start, start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = 'failed', audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z', gdrive_folder_id = 'folder-m2', sync_status = ?, sync_state = ? WHERE id = 'm2'`, storage.SyncSynced, storage.SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m3", []string{"m2", d}, start, start.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder-m3', sync_status = ?, sync_state = ?, refinement_status = 'failed' WHERE id = 'm3'`, storage.SyncSynced, storage.SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	audio := func(id string) *drive.File {
		return &drive.File{Name: recordingname.FileName(path(id)), MimeType: "audio/mpeg"}
	}
	source := &r11FolderDrive{
		files: map[string][]*drive.File{
			"folder-m2": {audio(a), audio(b), audio(c)},
			"folder-m3": {audio(d)},
		},
		downloads: map[string][]byte{
			"folder-m2/" + recordingname.FileName(path(a)): []byte("a"),
			"folder-m2/" + recordingname.FileName(path(b)): []byte("b"),
			"folder-m2/" + recordingname.FileName(path(c)): []byte("c"),
		},
	}
	session, err := store.GetSession("m3")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, source, "")
	}); err != nil {
		t.Fatalf("retry fails when the folder is two merges above the leaves: %v (downloads %v)", err, source.downloaded)
	}
}
