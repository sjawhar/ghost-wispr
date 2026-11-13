package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestRawEraseReservationWarningAppearsAndClears(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	reservation, err := erase.Reserve(filepath.Join(t.TempDir(), "session.pcm"))
	if err != nil {
		t.Fatal(err)
	}
	hiddenPath, err := store.ReserveRawSessionErase("session", reservation.OriginalPath(), reservation.HiddenPath())
	if err != nil {
		t.Fatal(err)
	}
	registry := newWarningsRegistry()
	if err := refreshRawEraseReservationWarnings(store, registry); err != nil {
		t.Fatal(err)
	}
	warnings := strings.Join(statusWarnings(t, registry), "\n")
	if !strings.Contains(warnings, "session") || !strings.Contains(warnings, "raw recording") {
		t.Fatalf("status warnings = %q, want the blocked raw restore for session", warnings)
	}
	if err := store.CancelRawSessionErase("session", hiddenPath); err != nil {
		t.Fatal(err)
	}
	if err := refreshRawEraseReservationWarnings(store, registry); err != nil {
		t.Fatal(err)
	}
	if warnings := statusWarnings(t, registry); len(warnings) != 0 {
		t.Fatalf("status warnings after reservation clears = %q, want none", warnings)
	}
}
