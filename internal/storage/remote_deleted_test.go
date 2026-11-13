package storage

import (
	"slices"
	"testing"
	"time"
)

// syncedSessionInFolder ends a summarized session id, synced into folder-1
// with documents verified, and older than any age limit below.
func syncedSessionInFolder(t *testing.T, store *SQLiteStore, id string) {
	t.Helper()
	started := time.Now().UTC().AddDate(0, 0, -60)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
		t.Fatalf("end: %v", err)
	}
	if err := store.UpdateSummaryAtGeneration(id, "Standup", "A summary.", SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	syncTo(t, store, id, SyncStateSynced, "folder-1", 0, "")
}

// MarkRemoteDeleted marks only a session still synced in the folder GC asked
// Drive about, so a change or a sync since is left as it is, and it reports
// whether it marked.
func TestMarkRemoteDeletedOnlyMarksASessionStillSyncedInThatFolder(t *testing.T) {
	const id = "20261106090000"
	t.Run("synced in that folder", func(t *testing.T) {
		store := newTestSQLiteStore(t)
		syncedSessionInFolder(t, store, id)
		marked, err := store.MarkRemoteDeleted(id, "folder-1", "the folder is in the trash")
		if err != nil || !marked {
			t.Fatalf("MarkRemoteDeleted = %v, %v; want true, nil", marked, err)
		}
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if sess.SyncState != SyncStateRemoteDeleted || sess.SyncStatus != SyncFailed || sess.GDriveFolderID != "folder-1" || sess.ErrorMessage != "the folder is in the trash" {
			t.Errorf("session %q/%q in %q, error %q; want REMOTE_DELETED in its folder, with why", sess.SyncState, sess.SyncStatus, sess.GDriveFolderID, sess.ErrorMessage)
		}
	})
	t.Run("changed since", func(t *testing.T) {
		store := newTestSQLiteStore(t)
		syncedSessionInFolder(t, store, id)
		// A title edit sends it back to pending sync.
		if err := store.UpdateTitle(id, "Planning"); err != nil {
			t.Fatalf("rename: %v", err)
		}
		marked, err := store.MarkRemoteDeleted(id, "folder-1", "the folder is in the trash")
		if err != nil || marked {
			t.Fatalf("MarkRemoteDeleted = %v, %v; want false, nil", marked, err)
		}
		if sess, err := store.GetSession(id); err != nil || sess.SyncState != SyncStatePending {
			t.Errorf("sync state %q (%v), want %q left as it is", sess.SyncState, err, SyncStatePending)
		}
	})
	t.Run("another folder", func(t *testing.T) {
		store := newTestSQLiteStore(t)
		syncedSessionInFolder(t, store, id)
		marked, err := store.MarkRemoteDeleted(id, "folder-2", "the folder is in the trash")
		if err != nil || marked {
			t.Fatalf("MarkRemoteDeleted = %v, %v; want false, nil", marked, err)
		}
		if sess, err := store.GetSession(id); err != nil || sess.SyncStatus != SyncSynced {
			t.Errorf("sync status %q (%v), want it left synced", sess.SyncStatus, err)
		}
	})
}

// The age policy lists no session whose Drive folder is in the trash or gone.
func TestGetGCEligibleSessionsSkipsARemoteDeletedSession(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106090000"
	syncedSessionInFolder(t, store, id)
	if _, err := store.MarkRemoteDeleted(id, "folder-1", "the folder is in the trash"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	// MarkRemoteDeleted also sets sync_status failed. Restore it so the
	// REMOTE_DELETED condition itself keeps the session from age-based GC.
	if _, err := store.DB().Exec(`UPDATE sessions SET sync_status = ? WHERE id = ?`, SyncSynced, id); err != nil {
		t.Fatalf("restore synced status: %v", err)
	}
	ids, err := store.GetGCEligibleSessions(30)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("eligible %v, want none", ids)
	}
}

func TestCountUnverifiedSessionsSkipsARemoteDeletedSession(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106090100"
	syncedSessionInFolder(t, store, id)
	if _, err := store.DB().Exec(`UPDATE sessions SET documents_unverified = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("mark unverified: %v", err)
	}
	if n, err := store.CountUnverifiedSessions(); err != nil || n != 1 {
		t.Fatalf("CountUnverifiedSessions before remote deletion = %d, %v; want 1", n, err)
	}
	if _, err := store.MarkRemoteDeleted(id, "folder-1", "the folder is in the trash"); err != nil {
		t.Fatalf("mark remote deleted: %v", err)
	}
	if n, err := store.CountUnverifiedSessions(); err != nil || n != 0 {
		t.Errorf("CountUnverifiedSessions after remote deletion = %d, %v; want 0", n, err)
	}
}

// A REMOTE_DELETED session is kept on this unit only while it still names
// audio here: one whose audio is gone holds no copy GC could free, so it is
// not counted. A synced session is never counted.
func TestCountUnitOnlySessionsCountsOnlyRemoteDeletedSessionsWithAudio(t *testing.T) {
	store := newTestSQLiteStore(t)
	const noAudio, withAudio, synced = "20261106090200", "20261106090300", "20261106090400"
	syncedSessionInFolder(t, store, noAudio)
	for _, id := range []string{withAudio, synced} {
		started := time.Now().UTC().AddDate(0, 0, -60)
		if err := store.CreateSession(id, started); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatalf("end: %v", err)
		}
		syncTo(t, store, id, SyncStateSynced, "folder-1", 0, "")
	}
	for _, id := range []string{noAudio, withAudio} {
		if marked, err := store.MarkRemoteDeleted(id, "folder-1", "the folder is gone"); err != nil || !marked {
			t.Fatalf("mark %s remote deleted: %v, %v", id, marked, err)
		}
	}
	if n, err := store.CountUnitOnlySessions(); err != nil || n != 1 {
		t.Errorf("CountUnitOnlySessions = %d, %v; want 1, the REMOTE_DELETED session with audio only", n, err)
	}
}

// TestDeleteSegmentKeepsARemoteDeletedSessionRemoteDeleted fails if a segment
// deletion moves a session whose Drive folder is in the trash or gone out of
// REMOTE_DELETED: the status warning stops counting it as kept on this unit
// only, and with sync off it stays uncounted. It also fails if the sweep no
// longer lists the session, so the Drive replacement would not reach it.
func TestDeleteSegmentKeepsARemoteDeletedSessionRemoteDeleted(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106090500"
	segmentTestSession(t, store, id)
	syncTo(t, store, id, SyncStateSynced, "folder-1", 0, "")
	if marked, err := store.MarkRemoteDeleted(id, "folder-1", "the folder is in the trash"); err != nil || !marked {
		t.Fatalf("mark remote deleted: %v, %v", marked, err)
	}
	before, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := store.CountUnitOnlySessions(); err != nil || n != 1 {
		t.Fatalf("control: CountUnitOnlySessions = %d, %v before the deletion; want 1", n, err)
	}
	segments, err := store.GetSegments(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedAudioTestSegment(t, store, id, segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	after, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.SyncState != SyncStateRemoteDeleted || after.SyncStatus != before.SyncStatus {
		t.Errorf("after the deletion: sync_state %q, sync_status %q; want %q and %q as before", after.SyncState, after.SyncStatus, SyncStateRemoteDeleted, before.SyncStatus)
	}
	if n, err := store.CountUnitOnlySessions(); err != nil || n != 1 {
		t.Errorf("CountUnitOnlySessions = %d, %v after the deletion; want 1, the REMOTE_DELETED session", n, err)
	}
	if ids, err := store.GetSessionsNeedingSync(); err != nil || !slices.Contains(ids, id) {
		t.Errorf("sessions needing sync = %v, %v; want the session that owes Drive a replacement", ids, err)
	}
}
