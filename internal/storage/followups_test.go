package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Review of #39, note 2: a backup that fails part way left its unfinished
// copy on disk until the next start. It is removed when the backup fails, and
// the last backup stays as it was.
func TestAFailedBackupLeavesNoUnfinishedCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	first, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	// A directory where the backup goes: the copy cannot replace it.
	backupPath := path + ".pre-migrate.bak"
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(backupPath, "kept"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := os.Stat(backupPath + ".part"); !os.IsNotExist(err) {
		t.Errorf("unfinished copy left beside the backup (stat: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(backupPath, "kept")); err != nil {
		t.Errorf("what was at the backup's place: %v", err)
	}
}

// Review of #39, note 4: earlier versions wrote why a summary failed into
// error_message, the sync's error. The first time this version opens such a
// database, each of those messages moves to summary_error unless a summary
// has since completed (then it goes), or summary_error already says why a
// later attempt failed (then that stays); a sync's error stays where it is.
func TestOldSummaryFailuresMoveOutOfTheSyncError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	started := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	rows := []struct{ id, status, errorMessage, summaryError string }{
		{"20260901100000", SummaryFailed, "summarization failed: model unavailable (503)", ""},
		{"20260901110000", SummaryFailed, "failed to get canonical transcript: database is locked", ""},
		{"20260901120000", SummaryCompleted, "failed to store summary: disk I/O error", ""},
		{"20260901130000", SummaryFailed, "upload summary.md: googleapi: Error 503", ""},
		// Left running or pending when the app stopped: the reason is kept.
		{"20260901140000", SummaryRunning, "summarization failed: model unavailable (503)", ""},
		{"20260901150000", SummaryPending, "failed to store summary: disk I/O error", ""},
		// A later attempt's reason is already in summary_error.
		{"20260901160000", SummaryFailed, "summarization failed: an older reason", "summarization failed: the latest reason"},
	}
	for i, r := range rows {
		if err := store.CreateSession(r.id, started.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.DB().Exec(`UPDATE sessions SET status = 'ended', summary_status = ?, error_message = ?, summary_error = ? WHERE id = ?`,
			r.status, r.errorMessage, r.summaryError, r.id); err != nil {
			t.Fatalf("set up %s: %v", r.id, err)
		}
	}
	// As an earlier version leaves the database: the move has not run.
	if _, err := store.DB().Exec(`DELETE FROM migrations WHERE name = 'move-summary-failures-to-summary-error'`); err != nil {
		t.Fatalf("forget the move: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	want := map[string]struct{ errorMessage, summaryError string }{
		"20260901100000": {"", "summarization failed: model unavailable (503)"},
		"20260901110000": {"", "failed to get canonical transcript: database is locked"},
		"20260901120000": {"", ""},
		"20260901130000": {"upload summary.md: googleapi: Error 503", ""},
		"20260901140000": {"", "summarization failed: model unavailable (503)"},
		"20260901150000": {"", "failed to store summary: disk I/O error"},
		"20260901160000": {"", "summarization failed: the latest reason"},
	}
	for id, w := range want {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if sess.ErrorMessage != w.errorMessage || sess.SummaryError != w.summaryError {
			t.Errorf("%s: error_message %q, summary_error %q; want %q, %q", id, sess.ErrorMessage, sess.SummaryError, w.errorMessage, w.summaryError)
		}
	}
}

// Review of #39, round 2, note 1: a retry of a REMOTE_DELETED session holds
// it once Drive shows its folder back; if the app stopped during that sync,
// startup recovery moved it to a retry, out of REMOTE_DELETED, though Drive
// may have lost the folder again meanwhile. A sync claimed from
// REMOTE_DELETED now goes back to REMOTE_DELETED, keeping its folder and
// retry count, and its next retry asks Drive again. Any other cut-off sync
// still goes to a retry.
func TestRecoveryPutsASyncClaimedFromRemoteDeletedBack(t *testing.T) {
	store := newTestSQLiteStore(t)
	const deleted, other = "20261106090000", "20261106100000"
	syncedSessionInFolder(t, store, deleted)
	if _, err := store.MarkRemoteDeleted(deleted, "folder-1", "the folder is in the trash"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if _, ok, err := store.ClaimSync(deleted, SyncStateRemoteDeleted, "retry", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("claim from REMOTE_DELETED = %v, %v", ok, err)
	}
	syncedSessionInFolder(t, store, other)
	if err := store.UpdateTitle(other, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	syncTo(t, store, other, SyncStateSyncing, "", 0, "")

	if _, err := store.RecoverInterruptedSyncs(5, time.Now().UTC()); err != nil {
		t.Fatalf("recover: %v", err)
	}

	sess, err := store.GetSession(deleted)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sess.SyncState != SyncStateRemoteDeleted || sess.SyncStatus != SyncFailed || sess.GDriveFolderID != "folder-1" || sess.RetryCount != 0 {
		t.Errorf("claimed from REMOTE_DELETED, after recovery: %q/%q in %q, retry %d; want %q/%q in folder-1, retry 0",
			sess.SyncState, sess.SyncStatus, sess.GDriveFolderID, sess.RetryCount, SyncStateRemoteDeleted, SyncFailed)
	}
	if sess, err = store.GetSession(other); err != nil || sess.SyncState != SyncStateRetryScheduled || sess.RetryCount != 1 {
		t.Errorf("claimed from PENDING, after recovery: %q, retry %d (%v); want %q, retry 1", sess.SyncState, sess.RetryCount, err, SyncStateRetryScheduled)
	}
	// A sync that ends clears where its claim came from: a later cut-off
	// sync of the session, claimed from elsewhere, goes to a retry.
	if _, ok, err := store.ClaimSync(deleted, SyncStateRemoteDeleted, "retry-2", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("claim again = %v, %v", ok, err)
	}
	if ok, err := store.EndSync(deleted, "retry-2", SyncStatePending, "folder-1", 0, time.Now().UTC(), "changed during the sync"); err != nil || !ok {
		t.Fatalf("end sync = %v, %v", ok, err)
	}
	syncTo(t, store, deleted, SyncStateSyncing, "", 0, "")
	if _, err := store.RecoverInterruptedSyncs(5, time.Now().UTC()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if sess, err = store.GetSession(deleted); err != nil || sess.SyncState != SyncStateRetryScheduled {
		t.Errorf("claimed from PENDING after an ended sync, after recovery: %q (%v); want %q", sess.SyncState, err, SyncStateRetryScheduled)
	}
}

// Review of #39, round 2, note 12: a session restored from Drive was stored
// synced with no sync state. It is stored SYNCED, and one an earlier version
// restored gets SYNCED the first time this version opens the database.
func TestARestoredSessionIsStoredSynced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	started := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	for _, id := range []string{"20260901100000", "20260901110000"} {
		if err := store.ImportSession(&Session{ID: id, Title: "Standup", StartedAt: started, EndedAt: &ended, Status: "ended",
			Summary: "A summary.", SummaryStatus: SummaryCompleted, SyncStatus: SyncSynced, GDriveFolderID: "folder-" + id}, nil, nil); err != nil {
			t.Fatalf("import %s: %v", id, err)
		}
	}
	if sess, err := store.GetSession("20260901100000"); err != nil || sess.SyncState != SyncStateSynced {
		t.Errorf("restored: sync state %q (%v), want %q", sess.SyncState, err, SyncStateSynced)
	}
	// As an earlier version restored it.
	if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = '' WHERE id = '20260901110000'`); err != nil {
		t.Fatalf("set up: %v", err)
	}
	if _, err := store.DB().Exec(`DELETE FROM migrations WHERE name = 'restored-sessions-synced'`); err != nil {
		t.Fatalf("forget the migration: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if store, err = NewSQLiteStore(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if sess, err := store.GetSession("20260901110000"); err != nil || sess.SyncState != SyncStateSynced {
		t.Errorf("restored by an earlier version: sync state %q (%v), want %q", sess.SyncState, err, SyncStateSynced)
	}
}
