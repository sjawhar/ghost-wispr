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

type r11FolderDrive struct {
	files      map[string][]*drive.File
	downloads  map[string][]byte
	downloaded []string
	calls      map[string]int
}

func (d *r11FolderDrive) ListFolderFiles(_ context.Context, folderID string) ([]*drive.File, error) {
	if d.calls != nil {
		d.calls[folderID]++
	}
	return d.files[folderID], nil
}

func (d *r11FolderDrive) DownloadRecording(_ context.Context, folderID, name, destination string) error {
	d.downloaded = append(d.downloaded, folderID+"/"+name)
	return os.WriteFile(destination, d.downloads[folderID+"/"+name], 0o600)
}

// TestR11RestoreFindsPreMergeSourceFolder fails if a recording removed before
// merge is not restored from its source session's own Drive folder.
func TestR11RestoreFindsPreMergeSourceFolder(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "source-folder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "source.mp3")
	if err := store.CreateSession("source", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("source", started.Add(time.Minute), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'source-folder' WHERE id = 'source'`); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"source"}, started, started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z', gdrive_folder_id = 'merged-folder' WHERE id = 'merged'`); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	name := recordingname.FileName(path)
	unrelated := "audio-unrelated.mp3"
	drive := &r11FolderDrive{
		files: map[string][]*drive.File{
			"source-folder": {
				{Name: name, MimeType: "audio/mpeg"},
				{Name: unrelated, MimeType: "audio/mpeg"},
			},
		},
		downloads: map[string][]byte{
			"source-folder/" + name:      []byte("source recording"),
			"source-folder/" + unrelated: []byte("unrelated recording"),
		},
	}
	if err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, drive, "")
	}); err != nil {
		t.Fatalf("restore source-folder recording = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source recording not restored: %v", err)
	}
	after, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if after.AudioPath != path {
		t.Fatalf("restored audio path = %q, want only source recording %q", after.AudioPath, path)
	}
	if _, err := os.Stat(filepath.Join(dir, "unrelated.mp3")); !os.IsNotExist(err) {
		t.Fatalf("unrelated source-folder recording was restored: %v", err)
	}
}

func TestR12RestoreSkipsLeafFolderWhenMergedFolderHasRecording(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "source-folder-skip.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "source.mp3")
	if err := store.CreateSession("source", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("source", started.Add(time.Minute), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'source-folder' WHERE id = 'source'`); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"source"}, started, started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z', gdrive_folder_id = 'merged-folder' WHERE id = 'merged'`); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	name := recordingname.FileName(path)
	source := &r11FolderDrive{
		files: map[string][]*drive.File{
			"merged-folder": {{Name: name, MimeType: "audio/mpeg"}},
		},
		downloads: map[string][]byte{"merged-folder/" + name: []byte("source recording")},
		calls:     map[string]int{},
	}
	if err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, source, "")
	}); err != nil {
		t.Fatal(err)
	}
	if source.calls["source-folder"] != 0 {
		t.Fatalf("listed leaf folder %d times although merged folder had its recording", source.calls["source-folder"])
	}
}
