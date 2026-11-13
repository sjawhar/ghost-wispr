package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestStartupCleanupLeavesReservedRawEraseOutsideTheAudioDirectory(t *testing.T) {
	audioDir, rawDir := t.TempDir(), t.TempDir()
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
	raw := filepath.Join(rawDir, "session.pcm")
	contents := []byte("raw recording protected after a failed restore")
	if err := os.WriteFile(raw, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	reservation, err := erase.Reserve(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveRawSessionErase("session", reservation.OriginalPath(), reservation.HiddenPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.Begin(); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(reservation.HiddenPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })

	if err := cleanupRecordingDirectories(store, audioDir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(reservation.HiddenPath()); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("startup cleanup changed reserved raw to %q (%v)", got, err)
	}
	got := make([]byte, len(contents))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, contents) {
		t.Fatalf("startup cleanup zeroed a reserved raw recording: %q", got)
	}
}
