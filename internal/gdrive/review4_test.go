package gdrive

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// sweepOnce syncs what the app's sweep (sweepDriveSync in cmd/ghost-wispr)
// syncs, retries due or not: the sessions needing a sync, then those with a
// scheduled retry.
func sweepOnce(t *testing.T, store *storage.SQLiteStore, o *Orchestrator) {
	t.Helper()
	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatal(err)
	}
	retries, err := store.GetSessionsBySyncState(storage.SyncStateRetryScheduled)
	if err != nil {
		t.Fatal(err)
	}
	for i := range retries {
		ids = append(ids, retries[i].ID)
	}
	for _, id := range ids {
		_ = o.SyncSession(context.Background(), id)
	}
}

// TestReview4ReplacementCutOffByARestartIsStillSwept fails if the app
// stopping while a replacement holds the session's sync claim leaves the
// session where the sweep never finishes it. The session's retries were used
// up before the deletion, and the old Drive files hold the deleted words.
func TestReview4ReplacementCutOffByARestartIsStillSwept(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	if _, err := fixture.store.DB().Exec(`UPDATE sessions SET retry_count = ? WHERE id = ?`, MaxSyncRetries, fixture.id); err != nil {
		t.Fatal(err)
	}
	// The replacement's claim, as orchestrator.go takes it, then the restart.
	if _, claimed, err := fixture.store.ClaimSync(fixture.id, storage.SyncStatePending, "replacement-claim", time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("ClaimSync = %v, %v", claimed, err)
	}
	if _, err := fixture.store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	sess, err := fixture.store.GetSession(fixture.id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.SyncState != storage.SyncStatePending || sess.SyncStatus != storage.SyncPending || sess.RetryCount != MaxSyncRetries {
		t.Errorf("after the restart: state=%q status=%q retry=%d, want the claimed-from %q, %q, retry %d kept",
			sess.SyncState, sess.SyncStatus, sess.RetryCount, storage.SyncStatePending, storage.SyncPending, MaxSyncRetries)
	}
	fixture.wire.allowDeletes = true
	for range 3 {
		sweepOnce(t, fixture.store, fixture.orchestrator)
	}
	if sess, err = fixture.store.GetSession(fixture.id); err != nil {
		t.Fatal(err)
	}
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) || sess.DriveReplacePending {
		t.Errorf("after a restart during the replacement, the sweep never finished it: sync_state=%q pending=%v, old files %v still on Drive (deleted %v)",
			sess.SyncState, sess.DriveReplacePending, fixture.oldIDs, fixture.wire.deletedIDs)
	}
}

// TestPendingReplacementInAnyUnsweptStateIsSwept fails if a session that
// still owes Drive a replacement can sit in a sync state the sweep never
// reads, so that only a manual retry would remove the deleted words.
func TestPendingReplacementInAnyUnsweptStateIsSwept(t *testing.T) {
	for _, state := range []string{storage.SyncStateFailed, storage.SyncStateRemoteDeleted} {
		t.Run(state, func(t *testing.T) {
			fixture := newReplacementFixture(t)
			fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
			if _, err := fixture.store.DB().Exec(`UPDATE sessions SET sync_state = ?, retry_count = ? WHERE id = ?`, state, MaxSyncRetries, fixture.id); err != nil {
				t.Fatal(err)
			}
			ids, err := fixture.store.GetSessionsNeedingSync()
			if err != nil {
				t.Fatal(err)
			}
			if len(ids) != 1 || ids[0] != fixture.id {
				t.Fatalf("sessions needing sync = %v, want the %s session that owes a replacement", ids, state)
			}
			fixture.wire.allowDeletes = true
			sweepOnce(t, fixture.store, fixture.orchestrator)
			if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
				t.Errorf("a %s session owing a replacement was not replaced by the sweep: deleted %v, want %v", state, fixture.wire.deletedIDs, fixture.oldIDs)
			}
		})
	}
}
