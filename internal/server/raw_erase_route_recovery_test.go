package server

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func routeMergedStore(t *testing.T, dbPath, audioDir string) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"source-a", "source-b"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), ""); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(audioDir, id+".pcm"), []byte("raw audio of "+id), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return store
}

type rowGoneStore struct{ *storage.SQLiteStore }

func (s rowGoneStore) DeleteSession(id, _ string) (string, error) {
	if _, err := s.DB().Exec(`DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return "", err
	}
	return "", sql.ErrNoRows
}

func TestDeleteSessionRouteRestoresMergedRawAfterStopBeforeCommit(t *testing.T) {
	audioDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := routeMergedStore(t, dbPath, audioDir)
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, audioDir))
	if err != nil {
		t.Fatal(err)
	}
	originalBegin := beginReservedErasing
	t.Cleanup(func() { beginReservedErasing = originalBegin })
	calls := 0
	beginReservedErasing = func(reservation erase.Reservation) (erase.Pending, error) {
		calls++
		pending, err := reservation.Begin()
		if calls == 2 {
			panic("stop after the first source raw recording is hidden")
		}
		return pending, err
	}
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/api/sessions/merged", nil))
	}()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := erase.Leftovers(audioDir); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source-a", "source-b"} {
		if got, err := os.ReadFile(filepath.Join(audioDir, id+".pcm")); err != nil || string(got) != "raw audio of "+id {
			t.Errorf("raw PCM for %s after the stop = %q (%v), want restored bytes", id, got, err)
		}
		if _, err := reopened.GetSession(id); err != nil {
			t.Errorf("source row %s after the stop = %v, want kept", id, err)
		}
	}
}

func TestDeleteSessionRouteReusesAReservedRawErase(t *testing.T) {
	audioDir := t.TempDir()
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
	raw := filepath.Join(audioDir, "session.pcm")
	contents := []byte("raw recording from a failed startup restore")
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
	finish := finishRawErasing
	t.Cleanup(func() { finishRawErasing = finish })
	finished := make(chan struct{})
	finishRawErasing = func(store SessionStore, reservations []rawEraseReservation) {
		finish(store, reservations)
		close(finished)
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, audioDir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/session", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("retry DELETE = %d %s, want 204", rr.Code, rr.Body.String())
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not erase its persisted hidden raw recording")
	}
	var reservations int
	if err := store.DB().QueryRow(`SELECT count(*) FROM raw_erase_reservations`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 {
		t.Fatalf("reservations after successful retry = %d, want 0", reservations)
	}
	got := make([]byte, len(contents))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatalf("retry left raw bytes %q, want zeroed", got)
	}
	if _, err := os.Stat(reservation.HiddenPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hidden raw after successful retry = %v, want removed", err)
	}
}

func TestDeleteSessionRetryKeepsItsReservationWhenTheRowDeleteRefuses(t *testing.T) {
	audioDir := t.TempDir()
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
	raw := filepath.Join(audioDir, "session.pcm")
	if err := os.WriteFile(raw, []byte("raw recording protected while retry is refused"), 0o600); err != nil {
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
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = 'session'`); err != nil {
		t.Fatal(err)
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, audioDir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/session", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("retry DELETE while replacement is owed = %d %s, want 409", rr.Code, rr.Body.String())
	}
	var reservations int
	if err := store.DB().QueryRow(`SELECT count(*) FROM raw_erase_reservations WHERE session_id = 'session'`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("reservations after refused retry = %d, want 1", reservations)
	}
	if _, err := os.Stat(reservation.HiddenPath()); err != nil {
		t.Fatalf("hidden raw after refused retry = %v, want kept", err)
	}
}

func TestDeleteSessionZeroesRawWhenItsRowDisappears(t *testing.T) {
	dir := t.TempDir()
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
	raw := filepath.Join(dir, "session.pcm")
	contents := []byte("raw audio whose row disappears during deletion")
	if err := os.WriteFile(raw, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	h, err := Handler(testStaticFS(t), NewHub(), rowGoneStore{store}, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/session", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("delete after row disappearance = %d %s, want 500", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(raw); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("raw audio after its row disappeared = %v, want erased", err)
	}
	got := make([]byte, len(contents))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatalf("raw audio after its row disappeared = %q, want zeroed", got)
	}
}

func TestDeleteSessionRefreshesRawWarningWhenFreshRestoreFails(t *testing.T) {
	dir := t.TempDir()
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
	raw := filepath.Join(dir, "session.pcm")
	if err := os.WriteFile(raw, []byte("raw audio whose restore is blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalBegin := beginReservedErasing
	t.Cleanup(func() { beginReservedErasing = originalBegin })
	beginReservedErasing = func(reservation erase.Reservation) (erase.Pending, error) {
		pending, err := originalBegin(reservation)
		if reservation.OriginalPath() == raw && err == nil {
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
		}
		return pending, err
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	refreshed := false
	h, err := Handler(testStaticFS(t), NewHub(), &deleteSessionErrorStore{SQLiteStore: store, err: errors.New("row delete failed")}, &ControlHooks{
		RefreshRawEraseWarnings: func() { refreshed = true },
	}, "", nil, configStoreWithAudioDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/session", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("delete with blocked restore = %d %s, want 500", rr.Code, rr.Body.String())
	}
	if !refreshed {
		t.Fatal("fresh raw restore failure did not refresh the status warning")
	}
}
