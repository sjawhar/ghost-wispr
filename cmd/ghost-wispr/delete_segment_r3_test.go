package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type byContentWords map[string][]audio.Word

func (b byContentWords) Words(_ context.Context, path string) ([]audio.Word, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return b[string(contents)], nil
}

func TestR3DeleteWhilePendingSplice(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, _ := newDeletionTestStore(t, recording)
	segs, _ := store.GetSegments("session")
	if err := store.DeleteSegment("session", segs[0].ID, storage.DeletedSpan{SessionID: "session", AudioPath: recording, Start: 0, End: 0.65,
		SHA256Before: hashBytesForDeleteTest(original), SHA256After: hashBytesForDeleteTest([]byte("first-cut"))}); err != nil {
		t.Fatal(err)
	}
	runner := segmentDeleteRunner{store: store, transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		}, splice: fakeSplice}
	err := runner.Delete(context.Background(), "session", segs[1].ID)
	contents, _ := os.ReadFile(recording)
	if !errors.Is(err, storage.ErrSegmentSplicePending) || !bytes.Equal(contents, original) {
		t.Fatal("second delete was not refused, or the recording changed")
	}
}

func TestR3RecoveryHoldsTheDeleteLock(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	segs, _ := store.GetSegments("session")
	if err := store.DeleteSegment("session", segmentID, storage.DeletedSpan{SessionID: "session", AudioPath: recording, Start: 1, End: 2,
		SHA256Before: hashBytesForDeleteTest(original), SHA256After: hashBytesForDeleteTest([]byte("spliced"))}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	gatedSplice := func(ctx context.Context, path string, s, e float64) (string, error) {
		close(entered)
		<-release
		return fakeSplice(ctx, path, s, e)
	}
	recDone := make(chan struct{})
	go func() {
		_, _ = recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, gatedSplice, nil, func(error) {})
		close(recDone)
	}()
	<-entered
	var reached atomic.Bool
	runner := segmentDeleteRunner{store: store, transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			reached.Store(true)
			return deletionRecordingSet{Paths: []string{recording}}, nil
		}, splice: fakeSplice}
	delDone := make(chan error, 1)
	go func() { delDone <- runner.Delete(context.Background(), "session", segs[0].ID) }()
	time.Sleep(300 * time.Millisecond)
	if reached.Load() {
		t.Fatal("live delete read recordings while recovery held the splice")
	}
	close(release)
	<-recDone
	<-delDone
}

func TestR3MergedBoundarySegment(t *testing.T) {
	dir := t.TempDir()
	r1 := filepath.Join(dir, "20261111120000.mp3")
	r2 := filepath.Join(dir, "20261111130000.mp3")
	_ = os.WriteFile(r1, []byte("r1"), 0o600)
	_ = os.WriteFile(r2, []byte("r2"), 0o600)
	store, segmentID, _ := newDeletionTestStore(t, r1+","+r2)
	words := byContentWords{
		"r1":      {{Text: "before", Start: 0.1, End: 0.2}, {Text: "bridge", Start: 0.3, End: 0.4}},
		"r2":      {{Text: "private", Start: 0.1, End: 0.2}, {Text: "deletion", Start: 0.3, End: 0.4}, {Text: "token", Start: 0.5, End: 0.6}, {Text: "after", Start: 1.1, End: 1.2}, {Text: "bridge", Start: 1.3, End: 1.4}},
		"spliced": {{Text: "after", Start: 0.3, End: 0.4}, {Text: "bridge", Start: 0.5, End: 0.6}},
	}
	runner := segmentDeleteRunner{store: store, transcriber: words,
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{r1, r2}}, nil
		}, splice: fakeSplice}
	if err := runner.Delete(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("boundary delete refused: %v", err)
	}
	if c, _ := os.ReadFile(r2); string(c) != "spliced" {
		t.Fatalf("second recording not spliced: %q", c)
	}
	if c, _ := os.ReadFile(r1); string(c) != "r1" {
		t.Fatalf("first recording changed: %q", c)
	}
}

func TestR3MergedBoundaryOverCutStillRefused(t *testing.T) {
	dir := t.TempDir()
	r1 := filepath.Join(dir, "20261111120000.mp3")
	r2 := filepath.Join(dir, "20261111130000.mp3")
	_ = os.WriteFile(r1, []byte("r1"), 0o600)
	_ = os.WriteFile(r2, []byte("r2"), 0o600)
	store, segmentID, _ := newDeletionTestStore(t, r1+","+r2)
	words := byContentWords{
		"r1":      {{Text: "before", Start: 0.1, End: 0.2}, {Text: "bridge", Start: 0.3, End: 0.4}},
		"r2":      {{Text: "private", Start: 0.1, End: 0.2}, {Text: "deletion", Start: 0.3, End: 0.4}, {Text: "token", Start: 0.5, End: 0.6}, {Text: "after", Start: 1.1, End: 1.2}, {Text: "bridge", Start: 1.3, End: 1.4}},
		"spliced": {},
	}
	runner := segmentDeleteRunner{store: store, transcriber: words,
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{r1, r2}}, nil
		}, splice: fakeSplice}
	if err := runner.Delete(context.Background(), "session", segmentID); err == nil {
		t.Fatal("over-cut of the after-neighbour accepted")
	}
}
