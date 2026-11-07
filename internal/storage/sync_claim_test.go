package storage

import (
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func unchangedSession(Session, []transcribe.Segment) bool { return true }

// syncState returns session id's sync state, folder and retry count.
func syncState(t *testing.T, store *SQLiteStore, id string) (state, folder string, retries int) {
	t.Helper()
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession %s: %v", id, err)
	}
	return session.SyncState, session.GDriveFolderID, session.RetryCount
}

// ClaimSync moves a session to SYNCING for one sync, only from the state
// the sync read and never from SYNCING, so two syncs of a session never run
// at once; it returns the session as claimed.
func TestClaimSync(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106140000"
	endedWith(t, store, id, "/audio/a.mp3", RefinementPending)
	at := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)

	if _, ok, err := store.ClaimSync(id, SyncStateFailed, "sync-1", at); err != nil || ok {
		t.Errorf("ClaimSync from a state the session is not in = %v, %v; want refused", ok, err)
	}
	session, ok, err := store.ClaimSync(id, SyncStatePending, "sync-1", at)
	if err != nil || !ok {
		t.Fatalf("ClaimSync = %v, %v; want claimed", ok, err)
	}
	if session.SyncState != SyncStateSyncing || session.SyncStatus != SyncPending || session.LastSyncAttempt == nil || !session.LastSyncAttempt.Equal(at) {
		t.Errorf("claimed session: %q/%q, last attempt %v; want SYNCING/pending at %v", session.SyncState, session.SyncStatus, session.LastSyncAttempt, at)
	}
	if _, ok, err := store.ClaimSync(id, SyncStateSyncing, "sync-2", at); err != nil || ok {
		t.Errorf("a second ClaimSync while a sync holds the session = %v, %v; want refused", ok, err)
	}
	if _, ok, err := store.ClaimSync("20261106990000", SyncStatePending, "sync-3", at); err != nil || ok {
		t.Errorf("ClaimSync of a missing session = %v, %v; want refused", ok, err)
	}
}

// MarkSynced marks a session synced only while the sync that claimed it
// still holds it: never one in another state, or claimed by another sync.
func TestMarkSyncedOnlyUnderItsClaim(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106140000"
	endedWith(t, store, id, "/audio/a.mp3", RefinementPending)
	if ok, err := store.MarkSynced(id, "sync-1", "folder-1", unchangedSession); err != nil || ok {
		t.Errorf("MarkSynced of a session no sync claimed = %v, %v; want refused", ok, err)
	}
	if _, ok, err := store.ClaimSync(id, SyncStatePending, "sync-1", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("ClaimSync = %v, %v", ok, err)
	}
	if ok, err := store.MarkSynced(id, "sync-2", "folder-1", unchangedSession); err != nil || ok {
		t.Errorf("MarkSynced under another sync's claim = %v, %v; want refused", ok, err)
	}
	if state, _, _ := syncState(t, store, id); state != SyncStateSyncing {
		t.Errorf("after the refused MarkSynced: %q, want %q", state, SyncStateSyncing)
	}
	if ok, err := store.MarkSynced(id, "sync-1", "folder-1", unchangedSession); err != nil || !ok {
		t.Fatalf("MarkSynced under its claim = %v, %v; want marked", ok, err)
	}
	if state, folder, _ := syncState(t, store, id); state != SyncStateSynced || folder != "folder-1" {
		t.Errorf("after MarkSynced: %q in %q, want synced in folder-1", state, folder)
	}
	if ok, err := store.MarkSynced(id, "sync-1", "folder-1", unchangedSession); err != nil || ok {
		t.Errorf("MarkSynced again after the sync ended = %v, %v; want refused", ok, err)
	}
}

// EndSync records how a sync that did not mark its session synced ended,
// only while that sync still holds the session, keeping the folder it made.
func TestEndSync(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106140000"
	endedWith(t, store, id, "/audio/a.mp3", RefinementPending)
	at := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	if ok, err := store.EndSync(id, "sync-1", SyncStateRetryScheduled, "folder-1", 1, at, "upload failed"); err != nil || ok {
		t.Errorf("EndSync of a session no sync claimed = %v, %v; want refused", ok, err)
	}
	if _, ok, err := store.ClaimSync(id, SyncStatePending, "sync-1", at); err != nil || !ok {
		t.Fatalf("ClaimSync = %v, %v", ok, err)
	}
	if ok, err := store.EndSync(id, "sync-2", SyncStateRetryScheduled, "folder-1", 1, at, "upload failed"); err != nil || ok {
		t.Errorf("EndSync under another sync's claim = %v, %v; want refused", ok, err)
	}
	for _, state := range []string{SyncStateSynced, SyncStateSyncing} {
		if ok, err := store.EndSync(id, "sync-1", state, "folder-1", 0, at, "not an end"); err == nil || ok {
			t.Errorf("EndSync to %s = %v, %v; want an error", state, ok, err)
		}
	}
	if sess, err := store.GetSession(id); err != nil || sess.SyncState != SyncStateSyncing {
		t.Fatalf("after EndSync to states a sync does not end in: %q, %v; want still SYNCING", sess.SyncState, err)
	}
	if ok, err := store.EndSync(id, "sync-1", SyncStateRetryScheduled, "folder-1", 1, at, "upload failed"); err != nil || !ok {
		t.Fatalf("EndSync under its claim = %v, %v; want recorded", ok, err)
	}
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.SyncState != SyncStateRetryScheduled || session.SyncStatus != SyncFailed || session.GDriveFolderID != "folder-1" ||
		session.RetryCount != 1 || session.ErrorMessage != "upload failed" || session.LastSyncAttempt == nil || !session.LastSyncAttempt.Equal(at) {
		t.Errorf("after EndSync: %+v; want RETRY_SCHEDULED/failed in folder-1, retry 1, the error and the attempt time", session)
	}
	if ok, err := store.EndSync(id, "sync-1", SyncStateFailed, "", 1, at, "again"); err != nil || ok {
		t.Errorf("EndSync again after the sync ended = %v, %v; want refused", ok, err)
	}
}

// At startup, every session an earlier run of the app left SYNCING (it
// stopped during the sync) goes back to a retry, keeping its folder: the
// cut-off sync counts as a failed attempt, so one with no retries left is
// FAILED. Sessions in any other state are left alone.
func TestRecoverInterruptedSyncs(t *testing.T) {
	store := newTestSQLiteStore(t)
	for id, retries := range map[string]int{"20261106140000": 0, "20261106140100": 5} {
		endedWith(t, store, id, "/audio/"+id+".mp3", RefinementPending)
		// As an earlier run left it: claimed by a sync that never ended,
		// with the retries it had already had.
		syncTo(t, store, id, SyncStateRetryScheduled, "folder-"+id, retries, "")
		syncTo(t, store, id, SyncStateSyncing, "", 0, "")
	}
	endedWith(t, store, "20261106140200", "/audio/c.mp3", RefinementPending)
	syncTo(t, store, "20261106140200", SyncStateSynced, "folder-c", 0, "")

	at := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	recovered, err := store.RecoverInterruptedSyncs(5, at)
	if err != nil {
		t.Fatalf("RecoverInterruptedSyncs: %v", err)
	}
	if len(recovered) != 2 {
		t.Errorf("recovered %v, want the two syncing sessions", recovered)
	}
	for id, want := range map[string]struct {
		state   string
		retries int
	}{
		"20261106140000": {SyncStateRetryScheduled, 1},
		"20261106140100": {SyncStateFailed, 5},
	} {
		session, err := store.GetSession(id)
		if err != nil {
			t.Fatalf("GetSession %s: %v", id, err)
		}
		if session.SyncState != want.state || session.RetryCount != want.retries || session.GDriveFolderID != "folder-"+id ||
			session.LastSyncAttempt == nil || !session.LastSyncAttempt.Equal(at) || session.ErrorMessage == "" {
			t.Errorf("%s after recovery: %q, retry %d, folder %q, last attempt %v, error %q; want %q, retry %d, its folder, %v and a reason",
				id, session.SyncState, session.RetryCount, session.GDriveFolderID, session.LastSyncAttempt, session.ErrorMessage, want.state, want.retries, at)
		}
		if _, ok, err := store.ClaimSync(id, want.state, "next-sync", at); err != nil || !ok {
			t.Errorf("ClaimSync of %s after recovery = %v, %v; want claimed", id, ok, err)
		}
	}
	if state, _, _ := syncState(t, store, "20261106140200"); state != SyncStateSynced {
		t.Errorf("a synced session after recovery: %q, want synced", state)
	}
}

// Under a sync gate, the age policy deletes a session only while it is
// synced, so a change after GC listed it (which sends it back to sync) keeps
// it.
func TestDeleteEndedSessionGatedOnSync(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106140000"
	syncedWith(t, store, id, "The first summary.", "folder-1")
	if err := store.UpdateTitle(id, "Planning"); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}
	if deleted, err := store.DeleteEndedSession(id, "/audio/"+id+".mp3", true); err != nil || deleted {
		t.Errorf("gated DeleteEndedSession of a session waiting to sync = %v, %v; want kept", deleted, err)
	}
	if deleted, err := store.DeleteEndedSession(id, "/audio/"+id+".mp3", false); err != nil || !deleted {
		t.Errorf("ungated DeleteEndedSession = %v, %v; want deleted", deleted, err)
	}
}
