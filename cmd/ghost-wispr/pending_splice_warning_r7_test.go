package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logtest"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestR7PendingSpliceRecoveryUsesEachSpansWarningKeyThroughStatus(t *testing.T) {
	store, first, _ := r4PendingStore(t)
	second := filepath.Join(filepath.Dir(first), "second.mp3")
	if err := addPendingSpliceRow(store, "session", second); err != nil {
		t.Fatal(err)
	}
	failSplice := func(context.Context, string, float64, float64) (string, error) {
		return "", errors.New("splicer unavailable")
	}

	outcomes, err := recoveryRunner(store, filepath.Dir(first), failSplice, nil).recoverPendingSplices(context.Background(), nil)
	if err != nil || len(outcomes) != 2 || outcomes[0].Recovered || outcomes[0].Err == nil || outcomes[1].Recovered || outcomes[1].Err == nil {
		t.Fatalf("outcomes=%v err=%v, want two failed spans", outcomes, err)
	}
	registry := newWarningsRegistry()
	applyPendingSpliceRecoveryWarnings(registry, outcomes)
	warnings := statusWarnings(t, registry)
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), filepath.Base(first)) || !strings.Contains(strings.Join(warnings, "\n"), filepath.Base(second)) {
		t.Fatalf("status warnings = %q, want one warning for each recording", warnings)
	}
}

func TestR7StartupPendingSpliceWarningsHaveDistinctStatusKeys(t *testing.T) {
	registry := newWarningsRegistry()
	recordPendingSpliceCleanupWarning(registry, errors.New("cleanup failed"))
	recordPendingSpliceRecoveryWarning(registry, errors.New("recovery failed"))
	warnings := strings.Join(statusWarnings(t, registry), "\n")
	if strings.Count(warnings, "\n") != 1 || !strings.Contains(warnings, "cleaned up") || !strings.Contains(warnings, "recovery failed") {
		t.Fatalf("status warnings = %q, want distinct cleanup and recovery warnings", warnings)
	}
}

func TestR7PendingSpliceFailureLogsItsSpan(t *testing.T) {
	registry := newWarningsRegistry()
	logged := logtest.Capture(t)

	span := storage.DeletedSpan{SessionID: "session-42", AudioPath: "/recordings/meeting.mp3"}
	applyPendingSpliceRecoveryWarnings(registry, []pendingSpliceOutcome{{Span: span, Err: errors.New("splicing failed")}})

	warned := logged.At(slog.LevelWarn)
	if len(warned) != 1 || warned[0].Message != "pending segment splice failed" {
		t.Fatalf("pending splice log = %+v, want one warning that the splice failed", logged.Records())
	}
	if got := warned[0].Attrs; got["session_id"] != span.SessionID || got["recording"] != span.AudioPath || got["error"] != "splicing failed" {
		t.Fatalf("pending splice warning attributes = %v, want session %s, recording %s and the error", got, span.SessionID, span.AudioPath)
	}
}

func addPendingSpliceRow(store *storage.SQLiteStore, sessionID, recording string) error {
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		return err
	}
	started := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	_, err := store.DB().Exec(`
		INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at)
		VALUES (?, ?, 1, 2, ?, ?, ?)`,
		sessionID, recording, hashBytesForDeleteTest(original), hashBytesForDeleteTest([]byte("spliced")), started.Format(time.RFC3339Nano))
	return err
}
