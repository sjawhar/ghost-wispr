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
