package session

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type r3GateSummarizer struct {
	entered chan struct{}
	release chan struct{}
	err     error
}

func (g *r3GateSummarizer) Summarize(_ context.Context, _, transcript string) (string, string, string, string, error) {
	if g.entered != nil {
		close(g.entered)
		<-g.release
	}
	return "Private deletion token", "About " + transcript, "default", "{}", g.err
}

type r3RecordingIndexer struct {
	store   *storage.SQLiteStore
	mu      sync.Mutex
	calls   []string
	entered chan struct{}
	release chan struct{}
	done    chan struct{}
}

func (r *r3RecordingIndexer) IndexSession(_ context.Context, sessionID, transcript string) error {
	r.mu.Lock()
	r.calls = append(r.calls, transcript)
	r.mu.Unlock()
	if r.entered != nil {
		close(r.entered)
		<-r.release
	}
	err := r.store.StoreEmbedding(sessionID, 0, []float32{1}, "h", "m")
	if r.done != nil {
		close(r.done)
	}
	return err
}

func (r *r3RecordingIndexer) IndexSessionAtGeneration(_ context.Context, sessionID, transcript string, generation int) error {
	r.mu.Lock()
	r.calls = append(r.calls, transcript)
	r.mu.Unlock()
	if r.entered != nil {
		close(r.entered)
		<-r.release
	}
	err := r.store.StoreEmbeddingAtGeneration(sessionID, 0, []float32{1}, "h", "m", generation)
	if r.done != nil {
		close(r.done)
	}
	return err
}

func r3SessionStore(t *testing.T) (*storage.SQLiteStore, int64) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "r3.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	_ = store.CreateSession("r3", started)
	for i, text := range []string{"before bridge", "private deletion token", "after bridge"} {
		_ = store.AppendSegment("r3", transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)})
	}
	_ = store.EndSession("r3", started.Add(3*time.Second), "recording.mp3")
	segs, _ := store.GetSegments("r3")
	return store, segs[1].ID
}

func deleteR3Session(t *testing.T, store *storage.SQLiteStore, id int64) {
	t.Helper()
	if err := deleteRefinedAudioForTest(t, store, "r3", id, storage.DeletedSpan{AudioPath: "recording.mp3", Start: 1, End: 2, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
}

func waitR3(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal(description)
	}
}

func TestR3StaleSummaryIsRefusedAndNotIndexed(t *testing.T) {
	store, id := r3SessionStore(t)
	summary := &r3GateSummarizer{entered: make(chan struct{}), release: make(chan struct{})}
	indexer := &r3RecordingIndexer{store: store}
	manager := NewManager(store, nil, summary, nil, nil, 0)
	manager.SetIndexer(indexer)
	done := make(chan struct{})
	go func() { manager.generateSummary(context.Background(), "r3", time.Now()); close(done) }()
	waitR3(t, summary.entered, "summarizer did not start")
	deleteR3Session(t, store, id)
	close(summary.release)
	waitR3(t, done, "summary did not finish")
	time.Sleep(300 * time.Millisecond)
	session, _ := store.GetSession("r3")
	embeddings, _ := store.GetEmbeddings("r3")
	if session.Title != "" || len(indexer.calls) != 0 || len(embeddings) != 0 {
		t.Fatal("stale summary or its index reached the store")
	}
}

func TestR3EmbeddingAfterSuccessfulSummaryRacesTheDelete(t *testing.T) {
	store, id := r3SessionStore(t)
	indexer := &r3RecordingIndexer{store: store, entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	manager := NewManager(store, nil, &r3GateSummarizer{}, nil, nil, 0)
	manager.SetIndexer(indexer)
	manager.generateSummary(context.Background(), "r3", time.Now())
	waitR3(t, indexer.entered, "indexer did not start before delete")
	deleteR3Session(t, store, id)
	close(indexer.release)
	waitR3(t, indexer.done, "indexer did not finish")
	embeddings, _ := store.GetEmbeddings("r3")
	if len(embeddings) != 0 {
		t.Fatalf("%d embedding(s) of the pre-delete transcript stored after the deletion", len(embeddings))
	}
}
