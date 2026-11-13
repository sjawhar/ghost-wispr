package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestR11SpliceQueriesUseInsertionOrder fails if optional fractional seconds
// in RFC3339Nano reorder pending or completed splice rows.
func TestR11SpliceQueriesUseInsertionOrder(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "splice-order.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		path, after, deletedAt string
		complete               int
	}{
		{"completed.mp3", "first", "2026-11-13T12:00:00Z", 1},
		{"completed.mp3", "second", "2026-11-13T12:00:00.1Z", 1},
		{"pending-a.mp3", "first-pending", "2026-11-13T12:00:00Z", 0},
		{"pending-b.mp3", "second-pending", "2026-11-13T12:00:00.1Z", 0},
	} {
		if _, err := store.DB().Exec(`INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at, splice_complete)
			VALUES('session', ?, 1, 2, 'before', ?, ?, ?)`, row.path, row.after, row.deletedAt, row.complete); err != nil {
			t.Fatal(err)
		}
	}
	completed, err := store.CompletedSplicesForRecording("session", "completed.mp3")
	if err != nil || len(completed) != 2 || completed[0].SHA256After != "first" || completed[1].SHA256After != "second" {
		t.Fatalf("completed splices = %+v, err=%v; want insertion order", completed, err)
	}
	pending, err := store.PendingSplices("")
	if err != nil || len(pending) != 2 || pending[0].AudioPath != "pending-a.mp3" || pending[1].AudioPath != "pending-b.mp3" {
		t.Fatalf("pending splices = %+v, err=%v; want insertion order", pending, err)
	}
	oldest, found, err := store.OldestPendingSplice("session")
	if err != nil || !found || !oldest.Equal(started) {
		t.Fatalf("oldest pending = %s, found:%t err:%v; want %s", oldest, found, err, started)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = 'session'`); err != nil {
		t.Fatal(err)
	}
	missing, err := store.MissingCompletedSplices("")
	if err != nil || len(missing) != 2 || missing[0].SHA256After != "first" || missing[1].SHA256After != "second" {
		t.Fatalf("missing completed splices = %+v, err=%v; want insertion order", missing, err)
	}
}
