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

func TestR7PendingSpliceWarningClearsAfterRecoveryThroughStatus(t *testing.T) {
	store, recording, _ := r4PendingStore(t)
	registry := newWarningsRegistry()
	warn, clear := pendingSpliceWarningCallbacks(registry)
	failSplice := func(context.Context, string, float64, float64) (string, error) {
		return "", errors.New("splicer unavailable")
	}
	if _, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, failSplice, nil, warn, clear); err != nil {
		t.Fatalf("recover pending splice: %v", err)
	}
	if warnings := statusWarnings(t, registry); len(warnings) != 1 || !strings.Contains(warnings[0], recording) {
		t.Fatalf("status warnings after failed recovery = %q, want the recording warning", warnings)
	}
	otherSpan := storage.DeletedSpan{SessionID: "other-session", AudioPath: "/recordings/other.mp3"}
	warn(otherSpan, errors.New("other recording failed"))

	if recovered, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, nil, warn, clear); err != nil || recovered != 1 {
		t.Fatalf("recovery = %d, %v; want one successful recovery", recovered, err)
	}
	if warnings := statusWarnings(t, registry); len(warnings) != 1 || !strings.Contains(warnings[0], "other recording failed") {
		t.Fatalf("status warnings after successful recovery = %q, want only the other recording warning", warnings)
	}
}

func TestR7PendingSpliceRecoveryUsesEachSpansWarningKeyThroughStatus(t *testing.T) {
	store, first, _ := r4PendingStore(t)
	second := filepath.Join(filepath.Dir(first), "second.mp3")
	if err := addPendingSpliceRow(store, "session", second); err != nil {
		t.Fatal(err)
	}
	registry := newWarningsRegistry()
	warn, clear := pendingSpliceWarningCallbacks(registry)
	genericWarning := func(span storage.DeletedSpan, _ error) {
		warn(span, errors.New("splicing failed"))
	}
	failSplice := func(context.Context, string, float64, float64) (string, error) {
		return "", errors.New("splicer unavailable")
	}
	if recovered, err := recoverPendingSplices(context.Background(), store, filepath.Dir(first), nil, failSplice, nil, genericWarning, clear); err != nil || recovered != 0 {
		t.Fatalf("recovery = %d, %v; want two warned failures", recovered, err)
	}
	if warnings := statusWarnings(t, registry); len(warnings) != 2 || warnings[0] != "A segment deletion needs audio recovery: splicing failed" || warnings[1] != warnings[0] {
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

func TestR7PendingSpliceWarningLogsItsSpan(t *testing.T) {
	registry := newWarningsRegistry()
	var output bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	warn, _ := pendingSpliceWarningCallbacks(registry)
	span := storage.DeletedSpan{SessionID: "session-42", AudioPath: "/recordings/meeting.mp3"}
	warn(span, errors.New("splicing failed"))

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
