package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func r4PendingStore(t *testing.T) (*storage.SQLiteStore, string, []int64) {
	t.Helper()
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	_ = os.WriteFile(recording, original, 0o600)
	store, segmentID, _ := newDeletionTestStore(t, recording)
	segs, _ := store.GetSegments("session")
	if err := store.DeleteSegment("session", segmentID, storage.DeletedSpan{SessionID: "session", AudioPath: recording, Start: 1, End: 2,
		SHA256Before: hashBytesForDeleteTest(original), SHA256After: hashBytesForDeleteTest([]byte("spliced"))}); err != nil {
		t.Fatal(err)
	}
	return store, recording, []int64{segs[0].ID, segs[2].ID}
}

func r4Runner(store *storage.SQLiteStore, recording string) segmentDeleteRunner {
	return segmentDeleteRunner{store: store, transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		}, splice: fakeSplice}
}

func TestR4DeleteDuringRecoveryRefineIs409(t *testing.T) {
	store, recording, ids := r4PendingStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	refine := func(context.Context, string, string, time.Time) error { close(entered); <-release; return nil }
	done := make(chan struct{})
	go func() {
		_, _ = recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, refine, nil)
		close(done)
	}()
	<-entered
	errc := make(chan error, 1)
	go func() { errc <- r4Runner(store, recording).Delete(context.Background(), "session", ids[0]) }()
	select {
	case err := <-errc:
		t.Logf("delete during refine: %v", err)
		if !errors.Is(err, storage.ErrSegmentSplicePending) {
			t.Fatalf("want splice pending, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delete blocked during recovery refine")
	}
	close(release)
	<-done
}

func TestR4RecoveryFlagClearedAfterRefineError(t *testing.T) {
	store, recording, ids := r4PendingStore(t)
	refine := func(context.Context, string, string, time.Time) error { return errors.New("batch transcription not configured") }
	_, _ = recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, refine, func(e error) { t.Logf("warn: %v", e) })
	pending, _ := store.HasPendingSplice("session")
	err := r4Runner(store, recording).Delete(context.Background(), "session", ids[0])
	t.Logf("pending row=%v; later delete err=%v", pending, err)
	if !pending && errors.Is(err, storage.ErrSegmentSplicePending) {
		t.Fatal("PROOF: no splice is pending, but the session refuses deletes as pending until restart (recovering flag leaked)")
	}
}

