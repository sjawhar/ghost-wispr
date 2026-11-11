package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestR8RecoveryResultsClearPendingWarningThroughStatus(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	registry := newWarningsRegistry()
	failSplice := func(context.Context, string, float64, float64) (string, error) {
		return "", errors.New("splicer unavailable")
	}

	recovered, failed, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, failSplice, nil)
	if err != nil || len(recovered) != 0 || len(failed) != 1 {
		t.Fatalf("failed recovery = recovered:%v failed:%v err:%v, want one failure", recovered, failed, err)
	}
	applyPendingSpliceRecoveryWarnings(registry, recovered, failed)
	if warnings := statusWarnings(t, registry); len(warnings) != 1 || !strings.Contains(warnings[0], "splicer unavailable") {
		t.Fatalf("status warnings after failed recovery = %q, want the pending-splice failure", warnings)
	}

	recovered, failed, err = recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, nil)
	if err != nil || len(recovered) != 1 || len(failed) != 0 {
		t.Fatalf("successful recovery = recovered:%v failed:%v err:%v, want one recovery", recovered, failed, err)
	}
	applyPendingSpliceRecoveryWarnings(registry, recovered, failed)
	if warnings := statusWarnings(t, registry); len(warnings) != 0 {
		t.Fatalf("status warnings after successful recovery = %q, want none", warnings)
	}
}

func TestR8RefinementFailureUsesItsOwnStatusWarning(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	registry := newWarningsRegistry()
	refinementErr := errors.New("batch transcription not configured")

	recovered, failed, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, func(context.Context, string, string, time.Time) error {
		return refinementErr
	})
	if err != nil || len(recovered) != 1 || len(failed) != 1 {
		t.Fatalf("refinement failure = recovered:%v failed:%v err:%v, want one recovered and one failed span", recovered, failed, err)
	}
	applyPendingSpliceRecoveryWarnings(registry, recovered, failed)
	warnings := statusWarnings(t, registry)
	if len(warnings) != 1 || strings.Contains(warnings[0], "needs audio recovery") || !strings.Contains(warnings[0], "refinement after segment deletion failed for session session") || !strings.Contains(warnings[0], refinementErr.Error()) {
		t.Fatalf("status warnings after refinement failure = %q, want the refinement warning without an audio-recovery claim", warnings)
	}
}
