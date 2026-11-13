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
}

func (d *r11FolderDrive) ListFolderFiles(_ context.Context, folderID string) ([]*drive.File, error) {
	return d.files[folderID], nil
}

func (d *r11FolderDrive) DownloadRecording(_ context.Context, folderID, name, destination string) error {
	d.downloaded = append(d.downloaded, folderID+"/"+name)
	return os.WriteFile(destination, d.downloads[folderID+"/"+name], 0o600)
}

// TestR11NestedOldMergeAcceptsLeafRecordings fails if an old nested merge
// treats its intermediate merged-session identifier as a recording.
func TestR11NestedOldMergeAcceptsLeafRecordings(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "nested.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ids := []string{"20261106100000", "20261106110000", "20261106120000"}
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	for i, id := range ids {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), filepath.Join(dir, id+".mp3")); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("m1", ids[:2], started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("top", []string{"m1", ids[2]}, started, started.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DELETE FROM recording_sources; UPDATE sessions SET audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z' WHERE id = 'top'`); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession("top")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(ids))
	for _, id := range ids {
		paths = append(paths, filepath.Join(dir, id+".mp3"))
	}
	if err := restoreRefinementRecording(context.Background(), store, &session, dir, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{Paths: paths, Downloaded: paths}, nil
	}); err != nil {
		t.Fatalf("complete nested restore = %v", err)
	}
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
	drive := &r11FolderDrive{
		files: map[string][]*drive.File{
			"source-folder": []*drive.File{{Name: name, MimeType: "audio/mpeg"}},
		},
		downloads: map[string][]byte{"source-folder/" + name: []byte("source recording")},
	}
	if err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, drive, "")
	}); err != nil {
		t.Fatalf("restore source-folder recording = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source recording not restored: %v", err)
	}
}
