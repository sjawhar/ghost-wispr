package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestR9RetryThatLeavesRefinementFailedKeepsStatusWarning(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	registry := newWarningsRegistry()
	span := storage.DeletedSpan{SessionID: "session", AudioPath: recording}
	applyPendingSpliceRecoveryWarnings(registry, []pendingSpliceOutcome{{
		Span:      span,
		Recovered: true,
		Err: refinementAfterSegmentDeletionError{
			sessionID: span.SessionID,
			err:       errors.New("batch transcription not configured"),
		},
	}})

	if err := retryRefinementWithWarningClear(context.Background(), registry, store, "session", func(context.Context, string, string, time.Time) error {
		return setRefinementStatusForWarningTest(store, storage.RefinementFailed)
	}); err != nil {
		t.Fatal(err)
	}
	if warnings := statusWarnings(t, registry); len(warnings) != 1 {
		t.Fatalf("status warnings after failed refinement retry = %q, want the warning kept", warnings)
	}
}

func TestR9MergedRefinementRetryClearsSessionWarning(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	const mergedAudio = "first.mp3,second.mp3"
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ? WHERE id = ?`, mergedAudio, "session"); err != nil {
		t.Fatal(err)
	}
	registry := newWarningsRegistry()
	span := storage.DeletedSpan{SessionID: "session", AudioPath: recording}
	applyPendingSpliceRecoveryWarnings(registry, []pendingSpliceOutcome{{
		Span:      span,
		Recovered: true,
		Err: refinementAfterSegmentDeletionError{
			sessionID: span.SessionID,
			err:       errors.New("batch transcription not configured"),
		},
	}})

	if err := retryRefinementWithWarningClear(context.Background(), registry, store, "session", func(context.Context, string, string, time.Time) error {
		return setRefinementStatusForWarningTest(store, storage.RefinementCompleted)
	}); err != nil {
		t.Fatal(err)
	}
	if warnings := statusWarnings(t, registry); len(warnings) != 0 {
		t.Fatalf("status warnings after successful merged refinement retry = %q, want none", warnings)
	}
}

func TestR9OneSessionHasOneRefinementStatusWarning(t *testing.T) {
	registry := newWarningsRegistry()
	refinementErr := refinementAfterSegmentDeletionError{sessionID: "session", err: errors.New("batch transcription not configured")}
	applyPendingSpliceRecoveryWarnings(registry, []pendingSpliceOutcome{
		{Span: storage.DeletedSpan{SessionID: "session", AudioPath: "first.mp3"}, Recovered: true, Err: refinementErr},
		{Span: storage.DeletedSpan{SessionID: "session", AudioPath: "second.mp3"}, Recovered: true, Err: refinementErr},
	})
	if warnings := statusWarnings(t, registry); len(warnings) != 1 {
		t.Fatalf("status warnings for one session's refinement failures = %q, want one", warnings)
	}
}

func setRefinementStatusForWarningTest(store *storage.SQLiteStore, status string) error {
	_, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, refinement_generation = deletion_generation WHERE id = ?`, status, "session")
	return err
}
