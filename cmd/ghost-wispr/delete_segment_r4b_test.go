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
	var warnings []error
	count, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, wrongSplice, nil, func(e error) { warnings = append(warnings, e) })
	if err != nil || count != 0 || len(warnings) == 0 {
		t.Fatalf("recovered=%d err=%v warnings=%v, want a warned, unrecovered row", count, err, warnings)
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
