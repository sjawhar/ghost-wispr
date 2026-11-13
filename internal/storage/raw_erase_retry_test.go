package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
)

func TestOpeningStoreKeepsAReservationWhenRawRestoreFails(t *testing.T) {
	audioDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(audioDir, "session.pcm")
	contents := []byte("raw recording whose restore is temporarily blocked")
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
	if err := os.Chmod(audioDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(audioDir, 0o700) })
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening after a failed raw restore = %v, want the app to start", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := os.Chmod(audioDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var reservations int
	if err := reopened.DB().QueryRow(`SELECT count(*) FROM raw_erase_reservations WHERE session_id = 'session'`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("reservation count after failed restore = %d, want 1", reservations)
	}
	if got, err := os.ReadFile(reservation.HiddenPath()); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("hidden raw after failed restore = %q (%v), want original bytes kept", got, err)
	}
}

func TestDeleteSessionAllowsASourceWhoseMergeParentIsGone(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("source", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("source", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"source"}, started, started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = 'merged'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSession("source", t.TempDir()); err != nil {
		t.Fatalf("delete source with a missing merge parent: %v", err)
	}
	if _, err := store.GetSession("source"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("source after deletion = %v, want no row", err)
	}
}

func TestRuntimeOrphansSkipsReservedRawErase(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "ghost-wispr.db"))
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
	raw := filepath.Join(dir, "session.pcm")
	contents := []byte("raw recording protected from runtime orphan cleanup")
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

	if err := store.eraseOrphans(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(reservation.HiddenPath()); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("runtime orphan cleanup changed reserved raw to %q (%v)", got, err)
	}
	if err := store.CancelRawSessionErase("session", reservation.HiddenPath()); err != nil {
		t.Fatal(err)
	}
	if err := store.eraseOrphans(); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(contents))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatalf("runtime orphan cleanup left unreserved raw bytes %q", got)
	}
}
