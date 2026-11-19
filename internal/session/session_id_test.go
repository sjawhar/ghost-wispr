package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// takenRecorder refuses to start a recording for a session whose id names a
// recording already on disk, as the audio recorder does, and records the
// sessions it started.
type takenRecorder struct {
	mu      sync.Mutex
	taken   map[string]bool
	started []string
}

func (r *takenRecorder) StartSession(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.taken[sessionID] {
		return fmt.Errorf("a recording of session %s is on disk: %w", sessionID, os.ErrExist)
	}
	r.started = append(r.started, sessionID)
	return nil
}

func (*takenRecorder) EndSession(string) (string, error) { return "", nil }

func (*takenRecorder) DiscardUnsaved() string { return "" }

func (*takenRecorder) HoldPreroll() {}

func (*takenRecorder) ReleasePreroll() {}

func openIDStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// endedSession creates session id, ended with recording, and returns it as
// the store has it.
func endedSession(t *testing.T, store *storage.SQLiteStore, id string, startedAt time.Time, recording string) storage.Session {
	t.Helper()
	if err := store.CreateSession(id, startedAt); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	if err := store.EndSession(id, startedAt.Add(time.Minute), recording); err != nil {
		t.Fatalf("end session %s: %v", id, err)
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// expectUnchanged fails the test unless session before.ID is as it was.
func expectUnchanged(t *testing.T, store *storage.SQLiteStore, before *storage.Session) {
	t.Helper()
	after, err := store.GetSession(before.ID)
	if err != nil {
		t.Fatalf("session %s is gone: %v", before.ID, err)
	}
	if after.Status != before.Status || after.AudioPath != before.AudioPath || !after.StartedAt.Equal(before.StartedAt) ||
		after.EndedAt == nil || before.EndedAt == nil || !after.EndedAt.Equal(*before.EndedAt) {
		t.Errorf("session %s changed: %+v, was %+v", before.ID, after, before)
	}
}

// A session start never reuses an id another session has. With the clock set
// back (a Pi that started before its clock was synchronized) the second a new
// session starts at can name a session that exists, in the store or as a
// recording on disk. The start takes the next free id, and the session that
// has the id keeps its row and its recording.
func TestASessionStartTakesTheNextIDWhenOneIsTaken(t *testing.T) {
	store := openIDStore(t)
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	const inStore, onDisk, free = "20261106140930", "20261106140931", "20261106140932"
	earlier := endedSession(t, store, inStore, started.Add(-time.Hour), "/audio/"+inStore+".mp3")
	recorder := &takenRecorder{taken: map[string]bool{onDisk: true}}
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour), 0)

	if err := manager.ensureSessionStarted(started); err != nil {
		t.Fatalf("a session start at a second whose id is taken = %v, want the next free id", err)
	}
	if id, _ := manager.ActiveSession(); id != free {
		t.Errorf("the session started as %q, want %q, the first id no session has", id, free)
	}
	if !slices.Equal(recorder.started, []string{free}) {
		t.Errorf("recordings started %v, want only %s", recorder.started, free)
	}
	expectUnchanged(t, store, &earlier)
	listed, err := store.GetSessionsByDate("2026-11-06", false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := range listed {
		ids = append(ids, listed[i].ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{inStore, free}) {
		t.Errorf("the day lists sessions %v, want %s and %s", ids, inStore, free)
	}
}

// A manual start refuses a taken id the same way.
func TestAManualStartTakesTheNextIDWhenOneIsTaken(t *testing.T) {
	store := openIDStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	taken := map[string]storage.Session{}
	for offset := -2; offset <= 30; offset++ {
		at := now.Add(time.Duration(offset) * time.Second)
		id := at.Format("20060102150405")
		taken[id] = endedSession(t, store, id, at, "/audio/"+id+".mp3")
	}
	manager := NewManager(store, &takenRecorder{}, nil, nil, NewDetector(time.Hour), 0)

	id, err := manager.ManualStartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("a manual start at a second whose id is taken = %v, want the next free id", err)
	}
	if _, reused := taken[id]; reused {
		t.Errorf("the manual start took id %s, which a session has", id)
	}
	for _, before := range taken {
		expectUnchanged(t, store, &before)
	}
}
