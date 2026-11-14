package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/api/drive/v3"
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

// TestR4RecoveryRemovesItsDownloadWhenResplicedHashDiffers fails if a
// recovery whose re-splice produces another hash leaves the recording it
// downloaded from Drive (the unspliced original, deleted segment included) at
// the recording's path.
func TestR4RecoveryRemovesItsDownloadWhenResplicedHashDiffers(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder' WHERE id = 'session'`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(recording); err != nil {
		t.Fatal(err)
	}
	drive := &deletionDriveFake{
		files:     []*drive.File{{Name: "audio-" + filepath.Base(recording), MimeType: "audio/mpeg"}},
		downloads: map[string][]byte{"audio-" + filepath.Base(recording): []byte("original")},
	}
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
	if len(drive.downloaded) != 1 {
		t.Fatalf("control: recovery downloaded %d recordings, want the one missing", len(drive.downloaded))
	}
	if _, err := os.Stat(recording); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the recording downloaded for the failed recovery is still at %s (%v)", recording, err)
	}
}
