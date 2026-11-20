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

func (*takenRecorder) HoldPreroll(time.Duration) {}

func (*takenRecorder) ReleasePreroll() {}

func (*takenRecorder) SetLookback(time.Duration) {}

func (*takenRecorder) Pending() time.Duration { return 0 }

func (*takenRecorder) SinceClose() (time.Duration, bool) { return 0, false }

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
	if err := store.EndSession(id, startedAt.Add(time.Minute), recording, ""); err != nil {
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

// gatedRecorder is a takenRecorder whose start of a taken id waits for
// release, after telling entered, so a test can look while a session start
// is under way.
type gatedRecorder struct {
	takenRecorder
	entered chan string
	release chan struct{}
}

func (r *gatedRecorder) StartSession(sessionID string) error {
	if r.taken[sessionID] {
		r.entered <- sessionID
		<-r.release
	}
	return r.takenRecorder.StartSession(sessionID)
}

// TestTheSettledSessionIsNoneWhileItsStartMayMoveIt fails if SettledSession
// names a session whose start is still under way: the start may find its id
// taken and move to the next, and what took the first id would name a
// session that never existed. ActiveSession names the candidate meanwhile.
func TestTheSettledSessionIsNoneWhileItsStartMayMoveIt(t *testing.T) {
	store := openIDStore(t)
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	const onDisk, free = "20261106140930", "20261106140931"
	recorder := &gatedRecorder{takenRecorder: takenRecorder{taken: map[string]bool{onDisk: true}}, entered: make(chan string, 1), release: make(chan struct{})}
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour), 0)
	done := make(chan error, 1)
	go func() { done <- manager.ensureSessionStarted(started) }()
	select {
	case <-recorder.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the session start never reached the recorder")
	}
	if id, _ := manager.ActiveSession(); id != onDisk {
		t.Fatalf("control: while the start is under way ActiveSession = %q, want the candidate %q", id, onDisk)
	}
	if id := manager.SettledSession(); id != "" {
		t.Errorf("while the start is under way SettledSession = %q, want none: the start may move it", id)
	}
	if id, starts := manager.SessionMark(); id != "" || starts != 0 {
		t.Errorf("while the start is under way SessionMark = (%q, %d), want (\"\", 0): no session and no settled start yet", id, starts)
	}
	close(recorder.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if id := manager.SettledSession(); id != free {
		t.Errorf("once the start is done SettledSession = %q, want %q", id, free)
	}
	if id, starts := manager.SessionMark(); id != free || starts != 1 {
		t.Errorf("once the start is done SessionMark = (%q, %d), want (%q, 1)", id, starts, free)
	}
	if id := manager.SessionOfStart(1); id != free {
		t.Errorf("SessionOfStart(1) = %q, want the id the start settled on, %q", id, free)
	}
}

// TestTheSessionStartedHookGetsTheSessionAsItStarted fails if the
// session-started hook does not run once for each start, automatic and
// manual, with the id the start settled on and its sequence number, which
// counts settled starts from 1 with no gaps: the voice binds a session to the
// exchange that opened just before it by that sequence.
func TestTheSessionStartedHookGetsTheSessionAsItStarted(t *testing.T) {
	store := openIDStore(t)
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	const onDisk, free = "20261106140930", "20261106140931"
	manager := NewManager(store, &takenRecorder{taken: map[string]bool{onDisk: true}}, nil, nil, NewDetector(time.Hour), 0)
	type start struct {
		id  string
		seq uint64
	}
	got := make(chan start, 4)
	manager.SetSessionStartedHook(func(id string, seq uint64) { got <- start{id, seq} })
	if err := manager.ensureSessionStarted(started); err != nil {
		t.Fatal(err)
	}
	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	manual, err := manager.ManualStartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	bySeq := map[uint64]string{}
	for range 2 {
		select {
		case s := <-got:
			bySeq[s.seq] = s.id
		case <-time.After(5 * time.Second):
			t.Fatalf("the hook ran for %v, want both starts %s (sequence 1) and %s (sequence 2)", bySeq, free, manual)
		}
	}
	if bySeq[1] != free || bySeq[2] != manual || len(bySeq) != 2 {
		t.Errorf("the hook ran for %v, want sequence 1 -> %s and sequence 2 -> %s", bySeq, free, manual)
	}
	select {
	case s := <-got:
		t.Errorf("the hook ran again, for %+v", s)
	case <-time.After(50 * time.Millisecond):
	}
	// The record keeps each start's id by its number, the ended one too.
	for seq, want := range map[uint64]string{1: free, 2: manual, 3: ""} {
		if id := manager.SessionOfStart(seq); id != want {
			t.Errorf("SessionOfStart(%d) = %q, want %q", seq, id, want)
		}
	}
	if id, starts := manager.SessionMark(); id != manual || starts != 2 {
		t.Errorf("SessionMark = (%q, %d), want (%q, 2)", id, starts, manual)
	}
}

// TestTheRecordOfStartsKeepsItsBoundAcrossTheCounterWrapping fails if the
// record SessionOfStart reads keeps more than startIDsKept ids once the start
// counter wraps past its largest value: the eviction skipped the starts
// numbered below startIDsKept, so the record grew by one each start after the
// wrap. It keeps the last startIDsKept starts on either side of the wrap.
func TestTheRecordOfStartsKeepsItsBoundAcrossTheCounterWrapping(t *testing.T) {
	manager := NewManager(openIDStore(t), &takenRecorder{}, nil, nil, NewDetector(time.Hour), 1)
	seed := ^uint64(0) - startIDsKept - 2 // the counter wraps at the 67th start
	manager.mu.Lock()
	manager.sessionSeq = seed
	manager.mu.Unlock()
	const total = 2*startIDsKept + 3
	base := time.Now().UTC().Add(-total * time.Second).Truncate(time.Second)
	ids := make(map[uint64]string, total)
	for i := range uint64(total) {
		if err := manager.ensureSessionStarted(base.Add(time.Duration(i) * time.Second)); err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
		id, seq := manager.SessionMark()
		if want := seed + i + 1; seq != want {
			t.Fatalf("start %d settled as number %d, want %d", i+1, seq, want)
		}
		ids[seq] = id
		if err := manager.endCurrentSession(context.Background()); err != nil {
			t.Fatalf("end %d: %v", i+1, err)
		}
	}
	manager.mu.Lock()
	kept := len(manager.startIDs)
	manager.mu.Unlock()
	if kept != startIDsKept {
		t.Errorf("after %d starts across the wrap the record keeps %d ids, want %d", total, kept, startIDsKept)
	}
	last := seed + total // wraps past the largest value
	for back := range uint64(total) {
		seq := last - back
		want := ids[seq]
		if back >= startIDsKept {
			want = ""
		}
		if got := manager.SessionOfStart(seq); got != want {
			t.Errorf("SessionOfStart(%d), %d starts back, = %q, want %q", seq, back, got, want)
		}
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
