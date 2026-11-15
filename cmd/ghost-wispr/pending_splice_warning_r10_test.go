package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func r10Warning(t *testing.T) (*warningsRegistry, *storage.SQLiteStore) {
	t.Helper()
	store, recording, _ := r4PendingStore(t)
	registry := newWarningsRegistry()
	span := storage.DeletedSpan{SessionID: "session", AudioPath: recording}
	applyPendingSpliceRecoveryWarnings(registry, []pendingSpliceOutcome{{Span: span, Recovered: true,
		Err: refinementAfterSegmentDeletionError{sessionID: "session", err: errors.New("batch transcription not configured")}}})
	return registry, store
}

// A recovered refined deletion has already rebuilt its transcript. Retrying
// refinement is a no-op and clears the stale recovery warning.
func TestR10RetrySkipsCompletedRefinedDeletion(t *testing.T) {
	registry, store := r10Warning(t)
	sess, _ := store.GetSession("session")
	if err := retryRefinementWithWarningClear(context.Background(), registry, store, "session", func(context.Context, string, string, time.Time) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if sess.RefinementStatus != storage.RefinementCompleted {
		t.Fatalf("refinement status before retry = %q, want completed", sess.RefinementStatus)
	}
	if w := statusWarnings(t, registry); len(w) != 0 {
		t.Fatalf("no-op retry kept a stale warning: %q", w)
	}
}

// A retry for a session that is not in the registry is a no-op.
func TestR10RetryForUnrelatedSessionLeavesOtherWarnings(t *testing.T) {
	registry, store := r10Warning(t)
	registry.Set("other-key", "other warning")
	_ = setRefinementStatusForWarningTest(store, storage.RefinementCompleted)
	if err := retryRefinementWithWarningClear(context.Background(), registry, store, "session", func(context.Context, string, string, time.Time) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if w := statusWarnings(t, registry); len(w) != 1 || w[0] != "other warning" {
		t.Fatalf("warnings = %q, want only the unrelated warning", w)
	}
}
