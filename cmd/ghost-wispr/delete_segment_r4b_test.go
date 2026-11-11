package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
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
