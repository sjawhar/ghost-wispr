package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// claimTestSession creates ended session id with a transcript in store and
// returns its transcript generation.
func claimTestSession(t *testing.T, store *SQLiteStore, id string) int {
	t.Helper()
	started := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment(id, transcribe.Segment{Text: "We talked about the bridge.", EndTime: 2, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	_, generation, err := store.Canonicalize(id)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

// TestABatchClaimAppliesOnlyWhileItIsTheSummaryAttempt fails if a batch
// job's claim of a session's summary (StartBatchSummary, kept with the job)
// stores its result or its failure after a newer summary attempt started on
// the session: an online summary (StartBackfillSummary, StartSummary) or an
// empty one (StartEmptySummary, StartEmptySummaryAtGeneration). It also fails
// if a claim still the session's attempt, including after a restart's
// recovery (RecoverInterruptedSummaries) made the summary pending, does not
// store its result or its failure, or if a later claim reads the same
// attempt as an earlier one.
func TestABatchClaimAppliesOnlyWhileItIsTheSummaryAttempt(t *testing.T) {
	newer := map[string]func(*SQLiteStore, string, int) error{
		"an online summary": func(s *SQLiteStore, id string, _ int) error {
			if _, err := s.RecoverInterruptedSummaries(); err != nil {
				return err
			}
			return s.StartBackfillSummary(id)
		},
		"a summary made again": func(s *SQLiteStore, id string, _ int) error { return s.StartSummary(id) },
		"an empty summary":     func(s *SQLiteStore, id string, _ int) error { return s.StartEmptySummary(id) },
		"an empty summary at its generation": func(s *SQLiteStore, id string, generation int) error {
			return s.StartEmptySummaryAtGeneration(id, generation)
		},
	}
	for name, start := range newer {
		t.Run("after "+name, func(t *testing.T) {
			store := newTestSQLiteStore(t)
			generation := claimTestSession(t, store, "s")
			attempt, err := store.StartBatchSummary("s")
			if err != nil {
				t.Fatal(err)
			}
			claim := SummaryClaim{Generation: generation, Attempt: attempt}
			if err := start(store, "s", generation); err != nil {
				t.Fatalf("the newer attempt: %v", err)
			}
			if err := store.UpdateSummaryAtGeneration("s", "Online", "The online summary.", SummaryCompleted, "default", "{}", "model-a", generation); err != nil {
				t.Fatalf("the newer attempt's summary: %v", err)
			}
			if err := store.UpdateSummaryAtClaim("s", "Batch", "The batch summary.", SummaryCompleted, "default", "{}", "model-b", claim); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("the old claim's result after %s = %v, want %v", name, err, sql.ErrNoRows)
			}
			if _, err := store.FailSummaryAtClaim("s", "summarization failed: the batch job expired", claim); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("the old claim's failure after %s = %v, want %v", name, err, sql.ErrNoRows)
			}
			sess, err := store.GetSession("s")
			if err != nil {
				t.Fatal(err)
			}
			if sess.Summary != "The online summary." || sess.SummaryStatus != SummaryCompleted || sess.SummaryError != "" {
				t.Errorf("after the old claim: summary %q (%s), error %q; want the newer attempt's, completed, no error", sess.Summary, sess.SummaryStatus, sess.SummaryError)
			}
		})
	}

	t.Run("control: still the attempt after a restart", func(t *testing.T) {
		store := newTestSQLiteStore(t)
		generation := claimTestSession(t, store, "s")
		first, err := store.StartBatchSummary("s")
		if err != nil {
			t.Fatal(err)
		}
		claim := SummaryClaim{Generation: generation, Attempt: first}
		if _, err := store.RecoverInterruptedSummaries(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FailSummaryAtClaim("s", "summarization failed: the batch job expired", claim); err != nil {
			t.Fatalf("the claim's failure, still the attempt: %v", err)
		}
		second, err := store.StartBatchSummary("s")
		if err != nil {
			t.Fatal(err)
		}
		if second == first {
			t.Fatalf("two claims read attempt %d", first)
		}
		claim.Attempt = second
		if err := store.UpdateSummaryAtClaim("s", "Batch", "The batch summary.", SummaryCompleted, "default", "{}", "model-b", claim); err != nil {
			t.Fatalf("the claim's result, still the attempt: %v", err)
		}
		if sess, _ := store.GetSession("s"); sess.Summary != "The batch summary." || sess.SummaryStatus != SummaryCompleted {
			t.Errorf("the claim's result: %q (%s); want it stored", sess.Summary, sess.SummaryStatus)
		}
	})
}

// TestABatchKeepsTheAttemptItClaimed fails if a batch job's record does not
// keep the summary attempt of each claim (RecordSummaryBatch,
// SummaryBatchSessions), or if a job this unit has no record of is not
// ErrUnknownSummaryBatch.
func TestABatchKeepsTheAttemptItClaimed(t *testing.T) {
	store := newTestSQLiteStore(t)
	generation := claimTestSession(t, store, "s")
	attempt, err := store.StartBatchSummary("s")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSummaryBatch("batches/one", "gemini", map[string]SummaryClaim{"s": {Generation: generation, Attempt: attempt}}); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.SummaryBatchSessions("batches/one")
	if err != nil {
		t.Fatal(err)
	}
	if got := sessions["s"]; got.Generation != generation || got.Attempt != attempt || attempt == 0 || got.State != "pending" {
		t.Fatalf("the batch's record of s = %+v; want generation %d, attempt %d (not 0), pending", got, generation, attempt)
	}
	if _, err := store.SummaryBatchProvider("batches/unknown"); !errors.Is(err, ErrUnknownSummaryBatch) {
		t.Fatalf("the provider of a job with no record: %v, want %v", err, ErrUnknownSummaryBatch)
	}
}

// TestAClaimRecordedBeforeAttemptsWereKeptAppliesUntilTheNextStart fails if
// a database whose sessions and summary_batches have no attempt column does
// not gain both at open; or if a batch's claim recorded before (attempt 0)
// does not apply while no summary started since, or still applies once one
// did.
func TestAClaimRecordedBeforeAttemptsWereKeptAppliesUntilTheNextStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var generations [2]int
	for i, id := range []string{"kept", "started"} {
		generations[i] = claimTestSession(t, store, id)
		if err := store.StartSummary(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`ALTER TABLE sessions DROP COLUMN summary_attempt`,
		`ALTER TABLE summary_batches DROP COLUMN attempt`,
		`INSERT INTO summary_batches(batch_name, session_id, generation) VALUES('batches/old', 'kept', 0), ('batches/old', 'started', 0)`,
	} {
		if _, err := store.DB().Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sessions, err := store.SummaryBatchSessions("batches/old")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartSummary("started"); err != nil {
		t.Fatal(err)
	}
	claim := sessions["kept"].SummaryClaim
	claim.Generation = generations[0]
	if err := store.UpdateSummaryAtClaim("kept", "Batch", "The batch summary.", SummaryCompleted, "default", "{}", "model-b", claim); err != nil {
		t.Errorf("a claim recorded before attempts were kept, with no summary started since: %v; want it stored", err)
	}
	claim = sessions["started"].SummaryClaim
	claim.Generation = generations[1]
	if err := store.UpdateSummaryAtClaim("started", "Batch", "The batch summary.", SummaryCompleted, "default", "{}", "model-b", claim); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a claim recorded before attempts were kept, after a summary started: %v; want %v", err, sql.ErrNoRows)
	}
}
