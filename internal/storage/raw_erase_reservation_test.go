package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
)

func TestOpeningStoreRestoresReservedRawEraseBeforeDeletingLeftovers(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost-wispr.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}

	raw := filepath.Join(dir, "session.pcm")
	contents := []byte("raw audio kept when the app stops before delete commits")
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got, err := os.ReadFile(raw); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("raw audio after startup recovery = %q (%v), want original bytes", got, err)
	}
	if _, err := os.Stat(reservation.HiddenPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hidden raw audio after startup recovery = %v, want removed", err)
	}
	if n := rawEraseReservationCount(t, reopened); n != 0 {
		t.Fatalf("raw erase reservations after startup recovery = %d, want 0", n)
	}
}

func TestOpeningStoreFinishesReservedRawEraseAfterDeleteCommits(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost-wispr.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}

	raw := filepath.Join(dir, "session.pcm")
	contents := []byte("raw audio erased after the database delete commits")
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
	if _, err := store.DeleteSession("session", dir); err != nil {
		t.Fatal(err)
	}
	if n := rawEraseReservationCount(t, store); n != 0 {
		t.Fatalf("raw erase reservations after the row delete commits = %d, want 0", n)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := os.Stat(reservation.HiddenPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hidden raw audio after deleted-session startup = %v, want removed", err)
	}
	got := make([]byte, len(contents))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatalf("raw audio after deleted-session startup = %q, want zeroed", got)
	}
	if n := rawEraseReservationCount(t, reopened); n != 0 {
		t.Fatalf("raw erase reservations after deleted-session startup = %d, want 0", n)
	}
}

func rawEraseReservationCount(t *testing.T, store *SQLiteStore) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM raw_erase_reservations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
