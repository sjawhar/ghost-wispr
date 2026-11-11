package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestR8RecoveryResultsClearPendingWarningThroughStatus(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	registry := newWarningsRegistry()
	failSplice := func(context.Context, string, float64, float64) (string, error) {
		return "", errors.New("splicer unavailable")
	}

	outcomes, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, failSplice, nil)
	if err != nil || len(outcomes) != 1 || outcomes[0].Recovered || outcomes[0].Err == nil {
		t.Fatalf("failed recovery = outcomes:%v err:%v, want one failed span", outcomes, err)
	}
	applyPendingSpliceRecoveryWarnings(registry, outcomes)
	if warnings := statusWarnings(t, registry); len(warnings) != 1 || !strings.Contains(warnings[0], "splicer unavailable") {
		t.Fatalf("status warnings after failed recovery = %q, want the pending-splice failure", warnings)
	}

	outcomes, err = recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, nil)
	if err != nil || len(outcomes) != 1 || !outcomes[0].Recovered || outcomes[0].Err != nil {
		t.Fatalf("successful recovery = outcomes:%v err:%v, want one recovered span", outcomes, err)
	}
	applyPendingSpliceRecoveryWarnings(registry, outcomes)
	if warnings := statusWarnings(t, registry); len(warnings) != 0 {
		t.Fatalf("status warnings after successful recovery = %q, want none", warnings)
	}
}

func TestR8RefinementFailureUsesItsOwnStatusWarning(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	registry := newWarningsRegistry()
	refinementErr := errors.New("batch transcription not configured: upstream failure 123")

	outcomes, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, func(context.Context, string, string, time.Time) error {
		return refinementErr
	})
	if err != nil || len(outcomes) != 1 || !outcomes[0].Recovered || outcomes[0].Err == nil {
		t.Fatalf("refinement failure = outcomes:%v err:%v, want one recovered, failed span", outcomes, err)
	}
	applyPendingSpliceRecoveryWarnings(registry, outcomes)
	warnings := statusWarnings(t, registry)
	if len(warnings) != 1 || strings.Contains(warnings[0], "needs audio recovery") || strings.Contains(warnings[0], "upstream failure 123") || !strings.Contains(warnings[0], "Refinement after segment deletion failed for session session") || !strings.Contains(warnings[0], "batch transcription not configured") {
		t.Fatalf("status warnings after refinement failure = %q, want a short refinement warning without an audio-recovery claim", warnings)
	}
}

func TestR8SuccessfulRefinementRetryClearsItsStatusWarning(t *testing.T) {
	registry := newWarningsRegistry()
	span := storage.DeletedSpan{SessionID: "session", AudioPath: "/recordings/session.mp3"}
	applyPendingSpliceRecoveryWarnings(registry, []pendingSpliceOutcome{{
		Span:      span,
		Recovered: true,
		Err: refinementAfterSegmentDeletionError{
			sessionID: span.SessionID,
			err:       errors.New("batch transcription not configured"),
		},
	}})
	if warnings := statusWarnings(t, registry); len(warnings) != 1 {
		t.Fatalf("status warnings before refinement retry = %q, want one", warnings)
	}

	store, _, _ := r4PendingStore(t)
	if err := retryRefinementWithWarningClear(context.Background(), registry, store, "session", func(context.Context, string, string, time.Time) error {
		return setRefinementStatusForWarningTest(store, storage.RefinementCompleted)
	}); err != nil {
		t.Fatal(err)
	}
	if warnings := statusWarnings(t, registry); len(warnings) != 0 {
		t.Fatalf("status warnings after successful refinement retry = %q, want none", warnings)
	}
}
