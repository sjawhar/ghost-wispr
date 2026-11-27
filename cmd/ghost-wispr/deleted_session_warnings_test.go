package main

import (
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/logtest"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestARawEraseLeftForADeletedSessionIsNotNamedByIt: when GC's erase of a
// deleted session's raw recording cannot finish, its reservation stays until
// the next start erases it, and GET /api/status says so. The warning names
// the reservation's hidden file, not the session, which is gone; the raw
// recording of a session that stays is still named by its session.
func TestARawEraseLeftForADeletedSessionIsNotNamedByIt(t *testing.T) {
	const gone, kept = "20261116090000", "20261116100000"
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	audioDir := t.TempDir()
	started := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	hidden := map[string]string{}
	for _, id := range []string{gone, kept} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), "", ""); err != nil {
			t.Fatal(err)
		}
		reservation, err := erase.Reserve(filepath.Join(audioDir, id+".pcm"))
		if err != nil {
			t.Fatal(err)
		}
		if hidden[id], err = store.ReserveRawSessionErase(id, reservation.OriginalPath(), reservation.HiddenPath()); err != nil {
			t.Fatal(err)
		}
	}
	// GC's age delete removes the rows and leaves the reservation, whose
	// erase did not finish.
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = ?`, gone); err != nil {
		t.Fatal(err)
	}
	registry := newWarningsRegistry()
	if err := refreshRawEraseReservationWarnings(store, registry); err != nil {
		t.Fatal(err)
	}
	warnings := strings.Join(statusWarnings(t, registry), "\n")
	if !strings.Contains(warnings, kept) {
		t.Fatalf("control: no warning names the kept session's reservation: %q", warnings)
	}
	if strings.Contains(warnings, gone) {
		t.Errorf("a warning names deleted session %s: %q", gone, warnings)
	}
	if !strings.Contains(warnings, filepath.Base(hidden[gone])) {
		t.Errorf("no warning names the deleted session's reservation %s: %q", filepath.Base(hidden[gone]), warnings)
	}
}

// TestTheStartUpSpliceWarningsNameNoRecording: a start-up cleanup or recovery
// of segment deletions that fails is a warning of the whole unit, which no
// delete clears. Its error can name a recording, which is named for its
// session; the warning says which step failed, and the log keeps why.
func TestTheStartUpSpliceWarningsNameNoRecording(t *testing.T) {
	const id = "20261116090000"
	registry := newWarningsRegistry()
	logged := logtest.Capture(t)
	recordPendingSpliceCleanupWarning(registry, errors.New("remove /audio/.splice-"+id+".mp3: permission denied"))
	recordPendingSpliceRecoveryWarning(registry, errors.New("open /audio/"+id+".mp3: no such file or directory"))
	warnings := statusWarnings(t, registry)
	if len(warnings) != 2 || warnings[0] == warnings[1] {
		t.Fatalf("control: want two distinct start-up warnings, got %q", warnings)
	}
	for _, warning := range warnings {
		if strings.Contains(warning, id) {
			t.Errorf("a start-up splice warning names session %s: %q", id, warning)
		}
	}
	warned := logged.At(slog.LevelWarn)
	var messages []string
	for _, record := range warned {
		messages = append(messages, record.Message)
	}
	if want := []string{"failed to clean leftover splice copies", "failed to recover pending segment splices"}; !slices.Equal(messages, want) {
		t.Fatalf("the log's warnings are %q, want %q; all records: %+v", messages, want, logged.Records())
	}
	for _, record := range warned {
		if cause, _ := record.Attrs["error"].(string); !strings.Contains(cause, id) {
			t.Errorf("the warning %q does not keep why its step failed: %+v", record.Message, record.Attrs)
		}
	}
}
