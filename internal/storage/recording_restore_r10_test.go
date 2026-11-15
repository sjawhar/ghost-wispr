package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestR10LatestRecordingSHA256UsesInsertionOrder fails if RFC3339Nano's
// optional fractional seconds make text ordering select an older cut.
func TestR10LatestRecordingSHA256UsesInsertionOrder(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "recording-restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("session", started.Add(time.Minute), "session.mp3"); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		hash, deletedAt string
	}{
		{"older", "2026-11-13T12:00:00Z"},
		{"latest", "2026-11-13T12:00:00.1Z"},
	} {
		if _, err := store.DB().Exec(`INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at, splice_complete)
			VALUES('session', 'session.mp3', 1, 2, 'before', ?, ?, 1)`, row.hash, row.deletedAt); err != nil {
			t.Fatal(err)
		}
	}
	hash, found, err := store.LatestRecordingSHA256("session", "session.mp3")
	if err != nil || !found || hash != "latest" {
		t.Fatalf("latest hash = %q, found:%t err:%v, want latest", hash, found, err)
	}
}
