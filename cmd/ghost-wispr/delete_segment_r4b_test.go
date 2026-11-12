package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestR4RecoveryKeepsRecordingWhenResplicedHashDiffers(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	wrongSplice := func(_ context.Context, path string, _, _ float64) (string, error) {
		if err := os.WriteFile(path, []byte("different-cut"), 0o600); err != nil {
			return "", err
		}
		return hashBytesForDeleteTest([]byte("different-cut")), nil
	}
	outcomes, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, wrongSplice, nil)
	if err != nil || len(outcomes) != 1 || outcomes[0].Recovered || outcomes[0].Err == nil {
		t.Fatalf("outcomes=%v err=%v, want one failed, unrecovered row", outcomes, err)
	}
	contents, _ := os.ReadFile(recording)
	if !bytes.Equal(contents, []byte("original")) {
		t.Fatalf("recording = %q, want the original bytes after a mismatched re-splice", contents)
	}
	if pending, _ := store.HasPendingSplice("session"); !pending {
		t.Fatal("mismatched re-splice marked the row complete")
	}
}

// heldDownloadFake downloads like deletionDriveFake and keeps a descriptor on
// each file it writes, so a test reads the file's inode after it is removed.
type heldDownloadFake struct {
	deletionDriveFake
	held []*os.File
}

func (f *heldDownloadFake) DownloadRecording(ctx context.Context, folderID, name, destination string) error {
	if err := f.deletionDriveFake.DownloadRecording(ctx, folderID, name, destination); err != nil {
		return err
	}
	held, err := os.Open(destination)
	if err != nil {
		return err
	}
	f.held = append(f.held, held)
	return nil
}

// TestR4RecoveryRemovesItsDownloadWhenResplicedHashDiffers fails if a
// recovery whose re-splice produces another hash leaves the recording it
// downloaded from Drive (the unspliced original, deleted segment included) at
// the recording's path, or removes it without overwriting its bytes.
func TestR4RecoveryRemovesItsDownloadWhenResplicedHashDiffers(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder' WHERE id = 'session'`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(recording); err != nil {
		t.Fatal(err)
	}
	drive := &heldDownloadFake{deletionDriveFake: deletionDriveFake{
		files:     []*drive.File{{Name: "audio-" + filepath.Base(recording), MimeType: "audio/mpeg"}},
		downloads: map[string][]byte{"audio-" + filepath.Base(recording): []byte("original")},
	}}
	t.Cleanup(func() {
		for _, f := range drive.held {
			_ = f.Close()
		}
	})
	wrongSplice := func(_ context.Context, path string, _, _ float64) (string, error) {
		if err := os.WriteFile(path, []byte("different-cut"), 0o600); err != nil {
			return "", err
		}
		return hashBytesForDeleteTest([]byte("different-cut")), nil
	}

	outcomes, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), drive, wrongSplice, nil)
	if err != nil || len(outcomes) != 1 || outcomes[0].Recovered || outcomes[0].Err == nil {
		t.Fatalf("outcomes=%v err=%v, want one failed, unrecovered row", outcomes, err)
	}
	if len(drive.held) != 1 {
		t.Fatalf("control: recovery downloaded %d recordings, want the one missing", len(drive.held))
	}
	if _, err := os.Stat(recording); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the recording downloaded for the failed recovery is still at %s (%v)", recording, err)
	}
	got := make([]byte, len("original"))
	if _, err := drive.held[0].ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Errorf("the removed download still holds %q", got)
	}
}

func TestR4DeleteRenamesAfterPostCommitMaintenanceError(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	runner := r4Runner(store, recording)
	runner.deleteSegment = func(sessionID string, id int64, span storage.DeletedSpan) error {
		if err := store.DeleteSegment(sessionID, id, span); err != nil {
			t.Fatal(err)
		}
		return storage.ErrSegmentPostCommitMaintenance
	}
	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, storage.ErrSegmentPostCommitMaintenance) {
		t.Fatalf("delete error = %v, want post-commit maintenance error", err)
	}
	contents, err := os.ReadFile(recording)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, []byte("spliced")) {
		t.Fatalf("recording = %q, want renamed splice despite maintenance error", contents)
	}
}
