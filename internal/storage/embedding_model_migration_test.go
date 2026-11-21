package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestAStoredEmbeddingsModelLosesItsOldVendorPrefix fails if a vector stored
// under a build that named its model gemini/<model> or openai/<model> keeps
// that name after an open: the indexer now names the model as its provider
// names it (gemini-embedding-2), whichever provider serves it, and would
// embed every such chunk again at the session's next index. A name in any
// other form is left as it is.
func TestAStoredEmbeddingsModelLosesItsOldVendorPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("s", time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for i, model := range []string{"gemini/gemini-embedding-2", "openai/text-embedding-3-small", "gemini-embedding-2", "unknown"} {
		if err := store.StoreEmbedding("s", i, []float32{1}, "hash", model); err != nil {
			t.Fatal(err)
		}
	}
	// The rows belong to a database that has not run this data migration.
	if _, err := store.DB().Exec(`DELETE FROM migrations WHERE name = 'embedding-model-provider-names'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	stored, err := reopened.GetEmbeddings("s")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gemini-embedding-2", "text-embedding-3-small", "gemini-embedding-2", "unknown"}
	if len(stored) != len(want) {
		t.Fatalf("%d embeddings after the reopen, want %d", len(stored), len(want))
	}
	for i, e := range stored {
		if e.Model != want[e.ChunkIndex] {
			t.Errorf("chunk %d: model %q, want %q", i, e.Model, want[e.ChunkIndex])
		}
	}
}
