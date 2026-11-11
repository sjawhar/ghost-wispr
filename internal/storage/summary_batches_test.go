package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSummaryBatchGenerationsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("s", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSummaryBatch("batches/one", map[string]int{"s": 7}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	generations, err := store.SummaryBatchGenerations("batches/one")
	if err != nil {
		t.Fatal(err)
	}
	if generations["s"] != 7 {
		t.Fatalf("persisted generations = %v, want session s at 7", generations)
	}
}
