package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/storeguard"
)

// cancelWatchingStore records each raw erase reservation cleared after the
// store was closed. The delete's own transaction already removed the
// reservation, so that DELETE changes no row and fires no trigger: storeguard
// cannot see it, and this wrapper does.
type cancelWatchingStore struct {
	*storage.SQLiteStore
	closed atomic.Bool
	late   atomic.Int32
}

func (s *cancelWatchingStore) CancelRawSessionErase(id, hiddenPath string) error {
	if s.closed.Load() {
		s.late.Add(1)
	}
	return s.SQLiteStore.CancelRawSessionErase(id, hiddenPath)
}

// TestADeletesRawEraseRunsAsWorkTheStopWaitsFor fails if the erase of a
// deleted session's raw recording, which a DELETE leaves running after its
// answer and which then clears its reservation in the store, touches the
// store after the app's stop has returned, when main closes the store: it
// must start through GoStoreWork, as main wires it to the session manager. It
// also fails if the stop does not wait for it within its deadline.
func TestADeletesRawEraseRunsAsWorkTheStopWaitsFor(t *testing.T) {
	dir := t.TempDir()
	sqlite, err := storage.NewSQLiteStore(filepath.Join(dir, "raw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	store := &cancelWatchingStore{SQLiteStore: sqlite}
	const id = "20261120090000"
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, at); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, at.Add(time.Minute), "", ""); err != nil {
		t.Fatal(err)
	}
	raw := recordingname.RawPath(dir, id)
	if err := os.WriteFile(raw, []byte("synthetic fixture; no room audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := storeguard.New(t, sqlite)
	manager := session.NewManager(sqlite, nil, nil, nil, session.NewDetector(time.Hour), 0)

	real := finishRawErasing
	t.Cleanup(func() { finishRawErasing = real })
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var hidden []string
	finishRawErasing = func(ctx context.Context, store SessionStore, reservations []rawEraseReservation) {
		defer close(finished)
		for _, r := range reservations {
			hidden = append(hidden, r.hiddenPath)
		}
		close(entered)
		select { // the erase's zeroing, which the stop cuts off
		case <-ctx.Done():
		case <-release:
		}
		real(ctx, store, reservations)
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{GoStoreWork: manager.Go}, "", nil, configStoreWithAudioDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+id, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", rec.Code, rec.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the delete started no raw erase")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.StopBackground(ctx); err != nil {
		t.Fatalf("the stop did not wait for the raw erase: %v", err)
	}
	store.closed.Store(true)
	guard.Close(t)
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the raw erase did not end")
	}
	time.Sleep(50 * time.Millisecond)
	guard.Check(t)
	if n := store.late.Load(); n != 0 {
		t.Errorf("the raw erase cleared %d reservations after the stop returned, when main closes the store", n)
	}
	if len(hidden) != 1 {
		t.Fatalf("control: %d raw captures hidden, want the session's one", len(hidden))
	}
	if _, err := os.Stat(raw); !os.IsNotExist(err) {
		t.Errorf("the raw capture is still under its name after the delete: %v", err)
	}
}
