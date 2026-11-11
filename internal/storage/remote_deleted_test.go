package storage

import (
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
	if err := store.UpdateSummary(id, "Standup", "A summary.", SummaryCompleted, "default", "{}"); err != nil {
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
