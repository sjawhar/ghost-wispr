package storage

import (
	"testing"
	"time"
)

// TestGCKeepsASessionThatOwesADriveReplacement fails if GC, with Drive sync
// off (no sync gate), may delete a session that still owes Drive a
// replacement, or remove its recording: the replacement needs both, and
// without them the Drive folder keeps the deleted words.
func TestGCKeepsASessionThatOwesADriveReplacement(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106100000"
	started := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), "/audio/recording.mp3"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET summary_status = ?, drive_replace_pending = 1 WHERE id = ?`, SummaryCompleted, id); err != nil {
		t.Fatal(err)
	}
	for _, syncGated := range []bool{false, true} {
		recordings, err := store.Recordings(syncGated)
		if err != nil {
			t.Fatal(err)
		}
		if len(recordings) != 1 || recordings[0].Removable {
			t.Errorf("sync gate %v: recordings = %+v, want the owed session's recording kept", syncGated, recordings)
		}
		deleted, err := store.DeleteEndedSession(id, "/audio/recording.mp3", syncGated)
		if err != nil {
			t.Fatal(err)
		}
		if deleted {
			t.Fatalf("sync gate %v: GC deleted a session that owes Drive a replacement", syncGated)
		}
	}
}
