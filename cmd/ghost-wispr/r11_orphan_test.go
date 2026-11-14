package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type failLaterDownloadDrive struct {
	files    []*drive.File
	contents map[string][]byte
	failName string
}

func (d *failLaterDownloadDrive) ListFolderFiles(context.Context, string) ([]*drive.File, error) {
	return d.files, nil
}

func (d *failLaterDownloadDrive) DownloadRecording(_ context.Context, _ string, name, destination string) error {
	if name == d.failName {
		return os.ErrPermission
	}
	return os.WriteFile(destination, d.contents[name], 0o600)
}

// TestR11RefusedRestoreRemovesDownloads fails if a rejected subset leaves
// recordings on the unit after the session keeps its empty audio path.
func TestR11RefusedRestoreRemovesDownloads(t *testing.T) {
	dir := t.TempDir()
	store, session, names := r9MergedRestoreSession(t, "")
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder', sync_status = ?, sync_state = ? WHERE id = 'merged'`, storage.SyncSynced, storage.SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	source := &deletionDriveFake{downloads: map[string][]byte{}}
	for _, name := range []string{names[0], names[2]} {
		driveName := recordingname.FileName(filepath.Join(dir, name))
		source.files = append(source.files, &drive.File{Name: driveName, MimeType: "audio/mpeg"})
		source.downloads[driveName] = []byte(name)
	}
	err = restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, source, "")
	})
	if err == nil {
		t.Fatal("control: subset restore succeeded")
	}
	for _, name := range []string{names[0], names[2]} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("refused restore left %s: %v", name, err)
		}
	}
}

func TestRestoreDeletionRecordingsCleansEarlierDownloadsOnALaterFailure(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.mp3")
	second := filepath.Join(dir, "second.mp3")
	source := &failLaterDownloadDrive{
		files: []*drive.File{
			{Name: recordingname.FileName(first), MimeType: "audio/mpeg"},
			{Name: recordingname.FileName(second), MimeType: "audio/mpeg"},
		},
		contents: map[string][]byte{recordingname.FileName(first): []byte("first")},
		failName: recordingname.FileName(second),
	}
	session := storage.Session{ID: "s1", GDriveFolderID: "folder"}
	if _, err := restoreDeletionRecordings(context.Background(), nil, &session, dir, source, ""); err == nil {
		t.Fatal("restore unexpectedly succeeded after a Drive download failed")
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("later download failure left the earlier recording: %v", err)
	}
}
