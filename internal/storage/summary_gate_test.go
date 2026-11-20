package storage

import (
	"errors"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/stopping"
)

// TestAClosedStoreRefusesEverySummaryWriteOfTheProviders fails if a write
// the providers change added (a batch job's claim, its record and its
// sessions' results, a result or failure at the claim, a backlog's start, the
// release of an attempt that stored nothing, the start's recovery of
// interrupted summaries, the labelled transcript a
// summary reads) reaches SQLite after Close, or fails with any error but
// stopping.ErrStopped: each must pass through the store's gate (storeGate),
// which the stop shuts, and not through a handle of its own.
func TestAClosedStoreRefusesEverySummaryWriteOfTheProviders(t *testing.T) {
	store, path := gatedStore(t)
	generation := claimTestSession(t, store, "s")
	attempt, err := store.StartBatchSummary("s")
	if err != nil {
		t.Fatal(err)
	}
	claim := SummaryClaim{Generation: generation, Attempt: attempt}
	if err := store.RecordSummaryBatch("batches/one", "gemini", map[string]SummaryClaim{"s": claim}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for name, write := range map[string]func() error{
		"StartBatchSummary": func() error { _, err := store.StartBatchSummary("s"); return err },
		"RecordSummaryBatch": func() error {
			return store.RecordSummaryBatch("batches/two", "gemini", map[string]SummaryClaim{"s": claim})
		},
		"MarkSummaryBatchSessionDone": func() error { return store.MarkSummaryBatchSessionDone("batches/one", "s", "succeeded") },
		"UpdateSummaryAtClaim": func() error {
			return store.UpdateSummaryAtClaim("s", "After", "Written after the close.", SummaryCompleted, "default", "{}", "model-a", claim)
		},
		"FailSummaryAtClaim":          func() error { _, err := store.FailSummaryAtClaim("s", "failed after the close", claim); return err },
		"StartBackfillSummary":        func() error { _, err := store.StartBackfillSummary("s"); return err },
		"ReleaseSummary":              func() error { _, err := store.ReleaseSummary("s", attempt); return err },
		"RecoverInterruptedSummaries": func() error { _, err := store.RecoverInterruptedSummaries(); return err },
		"CanonicalizeForSummary":      func() error { _, _, _, err := store.CanonicalizeForSummary("s"); return err },
	} {
		if err := write(); !errors.Is(err, stopping.ErrStopped) {
			t.Errorf("%s after Close = %v, want stopping.ErrStopped", name, err)
		}
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	sess, err := reopened.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if sess.SummaryStatus != SummaryRunning || sess.Summary != "" || sess.SummaryError != "" {
		t.Errorf("after writes refused at Close the session's summary is %q (%s), error %q; want it as the close left it, running", sess.Summary, sess.SummaryStatus, sess.SummaryError)
	}
	claims, err := reopened.SummaryBatchSessions("batches/one")
	if err != nil {
		t.Fatal(err)
	}
	if got := claims["s"]; len(claims) != 1 || got.Attempt != attempt || got.State != "pending" {
		t.Errorf("after writes refused at Close batches/one holds %+v; want s's claim at attempt %d, pending", claims, attempt)
	}
	if _, err := reopened.SummaryBatchProvider("batches/two"); !errors.Is(err, ErrUnknownSummaryBatch) {
		t.Errorf("a batch recorded after Close reads %v, want %v: none was recorded", err, ErrUnknownSummaryBatch)
	}
}
