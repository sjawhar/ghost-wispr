package storage

import (
	"errors"
	"testing"
	"time"
)

// RecordKeptRecording gives a session that ended with no recording the one
// saved from its kept raw recording: its refinement can run, and a synced
// session goes back to sync in its folder so Drive gets the recording. A
// sync state other than synced is left as it is.
func TestRecordKeptRecording(t *testing.T) {
	store := newTestSQLiteStore(t)
	const synced, remoteDeleted = "20261106140930", "20261106140931"
	for _, id := range []string{synced, remoteDeleted} {
		if err := store.CreateSession(id, time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RecoverStaleSessions(t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	syncTo(t, store, synced, SyncStateSynced, "folder-1", 0, "")
	syncTo(t, store, remoteDeleted, SyncStateRemoteDeleted, "folder-2", 0, "folder gone")

	for id, want := range map[string]struct{ state, status, folder string }{
		synced:        {SyncStatePending, SyncPending, "folder-1"},
		remoteDeleted: {SyncStateRemoteDeleted, SyncFailed, "folder-2"},
	} {
		placed := false
		recorded, err := store.RecordKeptRecording(id, "/audio/"+id+".mp3", func() error { placed = true; return nil })
		if !placed {
			t.Errorf("RecordKeptRecording(%s) recorded the recording without putting it in place", id)
		}
		if err != nil || !recorded {
			t.Fatalf("RecordKeptRecording(%s) = %v, %v; want recorded", id, recorded, err)
		}
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		if sess.AudioPath != "/audio/"+id+".mp3" || sess.RefinementStatus != RefinementPending {
			t.Errorf("%s: audio %q, refinement %q; want the recording and %q", id, sess.AudioPath, sess.RefinementStatus, RefinementPending)
		}
		if sess.SyncState != want.state || sess.SyncStatus != want.status || sess.GDriveFolderID != want.folder {
			t.Errorf("%s: sync %q/%q in %q, want %q/%q in %q", id, sess.SyncState, sess.SyncStatus, sess.GDriveFolderID, want.state, want.status, want.folder)
		}
	}
}

// RecordKeptRecording records nothing for a session that is not ended
// without a recording: one under way, one that has a recording, one merged,
// one whose audio GC removed to free space, or one that is gone.
func TestRecordKeptRecordingRefusesOtherSessions(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	const active, saved, removed, merged = "20261106100000", "20261106110000", "20261106120000", "20261106130000"
	if err := store.CreateSession(active, started); err != nil {
		t.Fatal(err)
	}
	endedWith(t, store, saved, "/audio/"+saved+".mp3", RefinementCompleted)
	endedAndSynced(t, store, removed, "/audio/"+removed+".mp3", RefinementCompleted)
	if _, ok, err := store.MarkAudioRemoved(removed, "/audio/"+removed+".mp3", "", false, started.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("MarkAudioRemoved = %v, %v", ok, err)
	}
	// A merged source names no recording: the merged session took it.
	endedWith(t, store, merged, "", RefinementNoAudio)
	if _, err := store.DB().Exec(`UPDATE sessions SET status = ?, merged_into = 'elsewhere' WHERE id = ?`, SessionMerged, merged); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{active, saved, removed, merged, "20261106990000"} {
		before, beforeErr := store.GetSession(id)
		placed := false
		if recorded, err := store.RecordKeptRecording(id, "/audio/kept.mp3", func() error { placed = true; return nil }); err != nil || recorded || placed {
			t.Errorf("RecordKeptRecording(%s) = %v, %v, placed %v; want nothing recorded and nothing put in place", id, recorded, err, placed)
		}
		after, afterErr := store.GetSession(id)
		if (beforeErr == nil) != (afterErr == nil) || before.AudioPath != after.AudioPath || before.SyncState != after.SyncState || before.RefinementStatus != after.RefinementStatus {
			t.Errorf("RecordKeptRecording(%s) changed the session: %+v -> %+v", id, before, after)
		}
	}
}

// A recording that cannot be put in place is not recorded: the session keeps
// no recording, for the next pass to try again.
func TestRecordKeptRecordingRecordsNothingWhenThePlaceFails(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106140930"
	if err := store.CreateSession(id, time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverStaleSessions(t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if recorded, err := store.RecordKeptRecording(id, "/audio/"+id+".mp3", func() error { return errors.New("no space left on device") }); err == nil || recorded {
		t.Errorf("RecordKeptRecording with a failed place = %v, %v; want an error and nothing recorded", recorded, err)
	}
	if sess, err := store.GetSession(id); err != nil || sess.AudioPath != "" || sess.RefinementStatus != RefinementNoAudio {
		t.Errorf("after a failed place the session names %q with refinement %q (%v), want nothing and %q", sess.AudioPath, sess.RefinementStatus, err, RefinementNoAudio)
	}
}
