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
	sessions, err := store.SummaryBatchSessions("batches/one")
	if err != nil {
		t.Fatal(err)
	}
	if sessions["s"].Generation != 7 || sessions["s"].State != "pending" {
		t.Fatalf("persisted sessions = %v, want session s at generation 7 and pending", sessions)
	}
}
