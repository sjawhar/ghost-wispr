package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
	recovered, failed, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, wrongSplice, nil)
	if err != nil || len(recovered) != 0 || len(failed) == 0 {
		t.Fatalf("recovered=%v failed=%v err=%v, want a failed, unrecovered row", recovered, failed, err)
	}
	contents, _ := os.ReadFile(recording)
	if !bytes.Equal(contents, []byte("original")) {
		t.Fatalf("recording = %q, want the original bytes after a mismatched re-splice", contents)
	}
	if pending, _ := store.HasPendingSplice("session"); !pending {
		t.Fatal("mismatched re-splice marked the row complete")
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
