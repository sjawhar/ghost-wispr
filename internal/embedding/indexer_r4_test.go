package embedding

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type gateClient struct {
	entered chan struct{}
	release chan struct{}
}

func (g *gateClient) Embed(_ context.Context, texts []string, _ TaskType) ([][]float32, error) {
	if g.entered != nil {
		close(g.entered)
		<-g.release
		g.entered = nil
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(i + 1)}
	}
	return out, nil
}

func r4Store(t *testing.T) (*storage.SQLiteStore, int64, int) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "r4.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	_ = store.CreateSession("s", started)
	texts := []string{"before bridge", "private deletion token", "after bridge"}
	for i, text := range texts {
		_ = store.AppendSegment("s", transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)})
	}
	_ = store.EndSession("s", started.Add(3*time.Second), "recording.mp3")
	words := make([]storage.RefinedWord, 0, len(texts))
	refined := make([]storage.RefinedSegment, 0, len(texts))
	for i, text := range texts {
		words = append(words, storage.RefinedWord{RecordingPath: "recording.mp3", RecordingSHA256: "b", Ordinal: i, Text: text, Start: float64(i), End: float64(i + 1)})
		refined = append(refined, storage.RefinedSegment{RecordingPath: "recording.mp3", RecordingSHA256: "b", FirstWordOrdinal: i, LastWordOrdinal: i, Text: text, Start: float64(i), End: float64(i + 1)})
	}
	revision, err := store.PublishRefinementAtGeneration("s", storage.RefinedTranscript(words), "test", words, refined, 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	return store, segments[1].ID, revision
}

func TestR4RealIndexerRefusesWriteAfterDelete(t *testing.T) {
	store, id, revision := r4Store(t)
	transcript, _, generation, err := store.GetCanonicalTranscriptWithGeneration("s")
	if err != nil {
		t.Fatal(err)
	}
	client := &gateClient{entered: make(chan struct{}), release: make(chan struct{})}
	idx := NewIndexer(client, store, 500)
	entered := client.entered
	errc := make(chan error, 1)
	go func() {
		errc <- idx.IndexSessionAtGeneration(context.Background(), "s", "before bridge private deletion token after bridge "+transcript, generation)
	}()
	<-entered
	span := storage.DeletedSpan{AudioPath: "recording.mp3", Start: 1, End: 2, SHA256Before: "b", SHA256After: "a"}
	if err := store.DeleteRefinedSegmentAudio("s", id, revision, &span); err != nil {
		t.Fatal(err)
	}
	close(client.release)
	ierr := <-errc
	embs, _ := store.GetEmbeddings("s")
	t.Logf("index err=%v embeddings after delete=%d", ierr, len(embs))
	if len(embs) != 0 {
		t.Fatalf("real indexer stored %d pre-delete chunk(s) after the delete", len(embs))
	}
}

func TestR4RealIndexerPrunesTrailingChunks(t *testing.T) {
	store, _, _ := r4Store(t)
	_, _, generation, _ := store.GetCanonicalTranscriptWithGeneration("s")
	idx := NewIndexer(&gateClient{}, store, 5)
	long := strings.Repeat("alpha beta gamma delta epsilon ", 6)
	if err := idx.IndexSessionAtGeneration(context.Background(), "s", long, generation); err != nil {
		t.Fatal(err)
	}
	before, _ := store.GetEmbeddings("s")
	if err := idx.IndexSessionAtGeneration(context.Background(), "s", "alpha beta gamma", generation); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetEmbeddings("s")
	t.Logf("chunks before=%d after=%d", len(before), len(after))
	if len(before) < 2 || len(after) != 1 {
		t.Fatalf("trailing chunks not pruned: before=%d after=%d", len(before), len(after))
	}
}

func TestR4BackfillRefusesWriteAfterDelete(t *testing.T) {
	store, id, revision := r4Store(t)
	client := &gateClient{entered: make(chan struct{}), release: make(chan struct{})}
	indexer := NewIndexer(client, store, 500)
	errc := make(chan error, 1)
	go func() { errc <- indexer.BackfillMissing(context.Background()) }()
	<-client.entered
	span := storage.DeletedSpan{AudioPath: "recording.mp3", Start: 1, End: 2, SHA256Before: "b", SHA256After: "a"}
	if err := store.DeleteRefinedSegmentAudio("s", id, revision, &span); err != nil {
		t.Fatal(err)
	}
	close(client.release)
	t.Logf("backfill result: %v", <-errc)
	embeddings, _ := store.GetEmbeddings("s")
	if len(embeddings) != 0 {
		t.Fatalf("backfill stored %d pre-delete embedding(s)", len(embeddings))
	}
}
