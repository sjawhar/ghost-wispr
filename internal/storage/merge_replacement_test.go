package storage

import (
	"errors"
	"testing"
	"time"
)

// TestMergeRefusesASourceThatOwesADriveReplacement fails if a session whose
// deleted segment Drive still holds can be merged: the merge would hide it
// from the sync sweep before its Drive files are replaced.
func TestMergeRefusesASourceThatOwesADriveReplacement(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b"} {
		if err := store.CreateSession(id, started.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Duration(i)*time.Hour+time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = 'b'`); err != nil {
		t.Fatal(err)
	}

	err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(2*time.Hour))
	if !errors.Is(err, ErrMergeReplacementPending) {
		t.Fatalf("MergeSessions with a source owing a replacement = %v, want %v", err, ErrMergeReplacementPending)
	}
	if _, err := store.GetSession("merged"); err == nil {
		t.Error("the refused merge created the merged session")
	}
	for _, id := range []string{"a", "b"} {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		if sess.Status != SessionEnded || sess.MergedInto != "" {
			t.Errorf("after the refused merge, %s is %q into %q, want it ended and unmerged", id, sess.Status, sess.MergedInto)
		}
	}
}

// TestAMergedSourceOwingADriveReplacementIsSwept fails if a source merged
// while it owed a replacement (by an earlier version, before merges were
// refused) is left out of the sync sweep, which reads ended sessions only.
func TestAMergedSourceOwingADriveReplacementIsSwept(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("source", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("source", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET status = 'merged', merged_into = 'elsewhere', drive_replace_pending = 1, sync_state = ? WHERE id = 'source'`, SyncStatePending); err != nil {
		t.Fatal(err)
	}
	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "source" {
		t.Errorf("sessions needing sync = %v, want the merged source that owes a replacement", ids)
	}
}

// TestEveryOwedSessionThatIsNotRecordingIsSwept fails if a session that owes
// Drive a replacement is left out of the sync sweep because it was discarded
// (consent to delete covers discarded sessions too), or if the sweep reads a
// session still recording.
func TestEveryOwedSessionThatIsNotRecordingIsSwept(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"discarded", "recording"} {
		if err := store.CreateSession(id, started.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET status = CASE id WHEN 'discarded' THEN 'discarded' ELSE 'active' END, drive_replace_pending = 1, sync_state = ?`, SyncStateFailed); err != nil {
		t.Fatal(err)
	}
	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "discarded" {
		t.Errorf("sessions needing sync = %v, want only the discarded session that owes a replacement", ids)
	}
}
