package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type r4GateEmbeddingClient struct {
	entered chan struct{}
	release chan struct{}
}

func (c *r4GateEmbeddingClient) Embed(_ context.Context, texts []string, _ embedding.TaskType) ([][]float32, error) {
	close(c.entered)
	<-c.release
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{1}
	}
	return vectors, nil
}

type r4NotifyingIndexer struct {
	indexer *embedding.Indexer
	done    chan struct{}
}

func (i *r4NotifyingIndexer) IndexSessionAtGeneration(ctx context.Context, sessionID, transcript string, generation int) error {
	err := i.indexer.IndexSessionAtGeneration(ctx, sessionID, transcript, generation)
	close(i.done)
	return err
}

func TestR4NoSummarizerRefusesEmbeddingAfterDelete(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "no-summarizer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"before bridge", "private deletion token", "after bridge"} {
		if err := store.AppendSegment("s", transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("s", started.Add(3*time.Second), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	client := &r4GateEmbeddingClient{entered: make(chan struct{}), release: make(chan struct{})}
	indexer := &r4NotifyingIndexer{indexer: embedding.NewIndexer(client, store, 500), done: make(chan struct{})}
	manager := NewManager(store, nil, nil, nil, nil, 0)
	manager.SetIndexer(indexer)
	manager.generateSummary(context.Background(), "s", started)
	<-client.entered
	if err := store.DeleteSegment("s", segments[1].ID, storage.DeletedSpan{AudioPath: "recording.mp3", Start: 1, End: 2, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
	close(client.release)
	<-indexer.done
	embeddings, err := store.GetEmbeddings("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(embeddings) != 0 {
		t.Fatalf("no-summarizer path stored %d pre-delete embedding(s)", len(embeddings))
	}
}

func TestR4RealIndexerRunsAfterSummaryFailure(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "failed-summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("s", transcribe.Segment{Text: "failure fallback transcript", StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Second), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	client := &r4GateEmbeddingClient{entered: make(chan struct{}), release: make(chan struct{})}
	indexer := &r4NotifyingIndexer{indexer: embedding.NewIndexer(client, store, 500), done: make(chan struct{})}
	manager := NewManager(store, nil, &r3GateSummarizer{err: errors.New("summary backend failed")}, nil, nil, 0)
	manager.SetIndexer(indexer)
	manager.generateSummary(context.Background(), "s", started)
	select {
	case <-client.entered:
	case <-time.After(time.Second):
		t.Fatal("failed summary did not start real-indexer fallback")
	}
	close(client.release)
	<-indexer.done
	session, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	embeddings, err := store.GetEmbeddings("s")
	if err != nil {
		t.Fatal(err)
	}
	if session.SummaryStatus != storage.SummaryFailed || len(embeddings) != 1 {
		t.Fatalf("failed summary status=%q embeddings=%d, want failed and one real-indexer chunk", session.SummaryStatus, len(embeddings))
	}
}
