package storage

import (
	"testing"
	"time"
)

// TestGCKeepsASessionThatOwesADriveReplacement fails if GC may delete a
// synced session that still owes Drive a replacement, or remove its
// recording: the replacement needs both, and without them the Drive folder
// keeps the deleted words. The session is synced, so only the owed
// replacement keeps it.
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
	if _, err := store.DB().Exec(`UPDATE sessions SET summary_status = ?, sync_status = ?, sync_state = ?, drive_replace_pending = 1 WHERE id = ?`, SummaryCompleted, SyncSynced, SyncStateSynced, id); err != nil {
		t.Fatal(err)
	}
	recordings, err := store.Recordings()
	if err != nil {
		t.Fatal(err)
	}
	if len(recordings) != 1 || recordings[0].Removable {
		t.Errorf("recordings = %+v, want the owed session's recording kept", recordings)
	}
	deleted, err := deleteEndedSessionForTest(store, id, "/audio/recording.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("GC deleted a session that owes Drive a replacement")
	}
}
