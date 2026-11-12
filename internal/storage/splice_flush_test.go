package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestPendingSplicesFlushesTheDirectoryBeforeMarkingASpliceComplete fails
// if PendingSplices marks complete a splice whose spliced recording is in
// place before the directory holding it is flushed: after a power loss the
// mark could stand beside the unspliced recording, which the Drive
// replacement would then upload as the clean copy.
func TestPendingSplicesFlushesTheDirectoryBeforeMarkingASpliceComplete(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "spliced")
	segments, err := store.GetSegments("spliced")
	if err != nil {
		t.Fatal(err)
	}
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	hash := func(contents string) string {
		sum := sha256.Sum256([]byte(contents))
		return hex.EncodeToString(sum[:])
	}
	if err := store.DeleteSegment("spliced", segments[1].ID, DeletedSpan{AudioPath: recording, Start: 2, End: 4, SHA256Before: hash("original"), SHA256After: hash("spliced")}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recording, []byte("spliced"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := flushRecordingDirectory
	t.Cleanup(func() { flushRecordingDirectory = original })
	flushed := 0
	flushRecordingDirectory = func(path string) error {
		flushed++
		if path != recording {
			t.Errorf("flushed the directory of %s, want %s", path, recording)
		}
		if pending, err := store.HasPendingSplice("spliced"); err != nil || !pending {
			t.Errorf("flushed the directory after the splice was marked complete (pending %v, %v)", pending, err)
		}
		return original(path)
	}

	pending, err := store.PendingSplices()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending splices = %+v, want the one in place completed", pending)
	}
	if flushed != 1 {
		t.Errorf("the recording directory was flushed %d times before the splice was marked complete, want 1", flushed)
	}
	if still, err := store.HasPendingSplice("spliced"); err != nil || still {
		t.Fatalf("control: splice still pending (%v, %v)", still, err)
	}
}
