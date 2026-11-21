package main

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// countedEmbeddings is an embedding client that counts its calls and answers
// each text with one vector.
type countedEmbeddings struct{ calls atomic.Int32 }

func (c *countedEmbeddings) Embed(_ context.Context, texts []string, _ embedding.TaskType) ([][]float32, error) {
	c.calls.Add(1)
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{0.1, 0.2}
	}
	return vectors, nil
}

// TestThePiConfigEmbedsNoStoredSessionAgain fails if a session the office Pi
// indexed under the build before named providers, which labelled its vectors
// gemini/gemini-embedding-2, is embedded again under the Pi's config
// (deploy/pi/files/ghost-wispr.yaml): the label the config's embedding.model
// gives a vector must be the one the database's relabel leaves on the old
// ones, or the deploy embeds every session again. The control, the same
// session indexed under the role's provider/model as its label, is embedded
// again, so the count can see a second embedding.
func TestThePiConfigEmbedsNoStoredSessionAgain(t *testing.T) {
	pi, _, err := config.Load(filepath.Join("..", "..", "deploy", "pi", "files", "ghost-wispr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, model, err := newEmbeddingClient(&pi)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("old", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("old", transcribe.Segment{Text: "The meeting chose the blue design.", EndTime: 2, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("old", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	transcript, generation, err := store.Canonicalize("old")
	if err != nil {
		t.Fatal(err)
	}
	calls := &countedEmbeddings{}
	before := embedding.NewIndexer(calls, store, 500)
	before.SetModel("gemini/gemini-embedding-2")
	if err := before.IndexSessionAtGeneration(t.Context(), "old", transcript, generation); err != nil || calls.calls.Load() != 1 {
		t.Fatalf("the earlier build's index: %d calls, %v; want one", calls.calls.Load(), err)
	}
	// The database is the earlier build's: it has not relabelled its vectors.
	if _, err := store.DB().Exec(`DELETE FROM migrations WHERE name = 'embedding-model-provider-names'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()

	for _, tc := range []struct {
		label string
		want  int32
	}{{model, 0}, {pi.Embedding.Model, 1}} {
		calls.calls.Store(0)
		indexer := newSearchIndexer(t.Context(), calls, tc.label, pi.Embedding.Model, reopened, newWarningsRegistry())
		if err := indexer.IndexSessionAtGeneration(t.Context(), "old", transcript, generation); err != nil {
			t.Fatal(err)
		}
		if got := calls.calls.Load(); got != tc.want {
			t.Errorf("under the label %q the session was embedded %d times, want %d", tc.label, got, tc.want)
		}
	}
}
