package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type r5Words map[string][]audio.Word

func (b r5Words) Words(_ context.Context, path string) ([]audio.Word, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return b[string(contents)], nil
}

func TestR5MergedBoundaryWithSharedWord(t *testing.T) {
	dir := t.TempDir()
	r1 := filepath.Join(dir, "20261111120000.mp3")
	r2 := filepath.Join(dir, "20261111130000.mp3")
	_ = os.WriteFile(r1, []byte("r1"), 0o600)
	_ = os.WriteFile(r2, []byte("r2"), 0o600)
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "r5.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	_ = store.CreateSession("session", started)
	for i, text := range []string{"we booked the room", "the private plan", "after bridge"} {
		_ = store.AppendSegment("session", transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)})
	}
	_ = store.EndSession("session", started.Add(3*time.Second), r1+","+r2)
	segs, _ := store.GetSegments("session")
	words := r5Words{
		"r1":      {{Text: "we", Start: 0.1, End: 0.2}, {Text: "booked", Start: 0.3, End: 0.4}, {Text: "the", Start: 0.5, End: 0.6}, {Text: "room", Start: 0.7, End: 0.8}},
		"r2":      {{Text: "the", Start: 0.1, End: 0.2}, {Text: "private", Start: 0.3, End: 0.4}, {Text: "plan", Start: 0.5, End: 0.6}, {Text: "after", Start: 1.1, End: 1.2}, {Text: "bridge", Start: 1.3, End: 1.4}},
		"spliced": {{Text: "after", Start: 0.3, End: 0.4}, {Text: "bridge", Start: 0.5, End: 0.6}},
	}
	runner := segmentDeleteRunner{store: store, transcriber: words,
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{r1, r2}}, nil
		}, splice: fakeSplice}
	if err := runner.Delete(context.Background(), "session", segs[1].ID); err != nil {
		t.Fatalf("PROOF: clean boundary cut refused because the segment shares \"the\" with a neighbour in the other recording: %v", err)
	}
}

func TestR5BatchReadOfUncanonicalizedMergedSession(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "r5b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b"} {
		at := started.Add(time.Duration(i) * time.Hour)
		_ = store.CreateSession(id, at)
		_ = store.AppendSegment(id, transcribe.Segment{Text: "words of " + id, StartTime: 0, EndTime: 1, Timestamp: at})
		_ = store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3")
	}
	if err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	segs, _ := store.GetSegments("merged")
	sessions, _, empty, err := readBatchSummarySessions(store, []string{"merged"})
	t.Logf("segments=%d batch sessions=%d empty=%v err=%v", len(segs), len(sessions), empty, err)
	if len(segs) > 0 && len(sessions) == 0 {
		t.Fatalf("PROOF: a merged session with %d segments is submitted as empty and marked completed with no summary", len(segs))
	}
}

func TestR5DeleteDuringRecoverySpliceIs409(t *testing.T) {
	store, recording, ids := r4PendingStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	gatedSplice := func(ctx context.Context, path string, start, end float64) (string, error) {
		close(entered)
		<-release
		return fakeSplice(ctx, path, start, end)
	}
	recovered := make(chan struct{})
	go func() {
		_, _, _ = recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, gatedSplice, nil)
		close(recovered)
	}()
	<-entered
	runner := r4Runner(store, recording)
	deleteResult := make(chan error, 1)
	go func() { deleteResult <- runner.Delete(context.Background(), "session", ids[0]) }()
	select {
	case err := <-deleteResult:
		if !errors.Is(err, storage.ErrSegmentSplicePending) {
			t.Fatalf("delete during splice = %v, want splice pending", err)
		}
	case <-time.After(time.Second):
		t.Fatal("delete blocked during the splice instead of returning 409")
	}
	close(release)
	<-recovered
}

func TestR5DeleteHookReturnsNotFoundBeforeBatchConfiguration(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "r5c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Second), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := requireSegmentForDeletion(store, "session", 42); !errors.Is(err, storage.ErrSegmentNotFound) {
		t.Fatalf("unconfigured delete lookup error = %v, want segment not found", err)
	}
}
