package storage

import (
	"errors"
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
	if err := store.RecordSummaryBatch("batches/one", "gemini", map[string]SummaryClaim{"s": {Generation: 7, Attempt: 3}}); err != nil {
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
	if sessions["s"].Generation != 7 || sessions["s"].Attempt != 3 || sessions["s"].State != "pending" {
		t.Fatalf("persisted sessions = %v, want session s at generation 7, attempt 3 and pending", sessions)
	}
	if provider, err := store.SummaryBatchProvider("batches/one"); err != nil || provider != "gemini" {
		t.Fatalf("the provider that took the job = %q, %v; want gemini", provider, err)
	}
	if _, err := store.SummaryBatchProvider("batches/unknown"); !errors.Is(err, ErrUnknownSummaryBatch) {
		t.Fatalf("the provider of a job with no record: %v; want %v", err, ErrUnknownSummaryBatch)
	}
}

// TestABatchRecordedBeforeItsProviderWasKeptReadsNone fails if a database
// whose summary_batches table has no provider column does not gain it at
// open, with "" for the jobs it holds.
func TestABatchRecordedBeforeItsProviderWasKeptReadsNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("s", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`ALTER TABLE summary_batches DROP COLUMN provider`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO summary_batches(batch_name, session_id, generation) VALUES('batches/old', 's', 3)`); err != nil {
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
	if provider, err := store.SummaryBatchProvider("batches/old"); err != nil || provider != "" {
		t.Fatalf("the provider of a job recorded before it was kept = %q, %v; want none", provider, err)
	}
}
