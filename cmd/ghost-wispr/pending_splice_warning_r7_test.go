package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	outcomes, err := recoverPendingSplices(context.Background(), store, filepath.Dir(first), nil, failSplice, nil)
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
	warnings := statusWarnings(t, registry)
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), "cleanup failed") || !strings.Contains(strings.Join(warnings, "\n"), "recovery failed") {
		t.Fatalf("status warnings = %q, want distinct cleanup and recovery warnings", warnings)
	}
}

func TestR7PendingSpliceFailureLogsItsSpan(t *testing.T) {
	registry := newWarningsRegistry()
	var output bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	span := storage.DeletedSpan{SessionID: "session-42", AudioPath: "/recordings/meeting.mp3"}
	applyPendingSpliceRecoveryWarnings(registry, []pendingSpliceOutcome{{Span: span, Err: errors.New("splicing failed")}})

	if entry := output.String(); !strings.Contains(entry, span.SessionID) || !strings.Contains(entry, span.AudioPath) {
		t.Fatalf("pending splice log = %q, want session and recording", entry)
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
