package gdrive

import (
	"context"
	"os"
	"strings"
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
// reads, so that only a manual retry would remove the deleted words, or if
// the finished replacement leaves the session in that state rather than
// PENDING, where the next sync marks it synced.
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
			sess, err := fixture.store.GetSession(fixture.id)
			if err != nil {
				t.Fatal(err)
			}
			if sess.SyncState != storage.SyncStatePending || sess.DriveReplacePending {
				t.Errorf("after the replacement of a %s session: state=%q pending=%v, want %q with nothing owed", state, sess.SyncState, sess.DriveReplacePending, storage.SyncStatePending)
			}
		})
	}
}

// TestReplacementWaitsForATrashedFolderAndRunsOnceItIsBack fails if a
// replacement uploads into, or deletes from, a session folder in the Drive
// trash, or if the session then waits for a manual retry once the folder is
// restored.
func TestReplacementWaitsForATrashedFolderAndRunsOnceItIsBack(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	fixture.wire.trash(fixture.folder)
	uploads := fixture.wire.uploads

	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err == nil {
		t.Fatal("a replacement into a folder in the trash reported success")
	}
	sess, err := fixture.store.GetSession(fixture.id)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.wire.uploads != uploads || len(fixture.wire.deletedIDs) != 0 {
		t.Errorf("with its folder in the trash, the replacement uploaded %d files and deleted %v, want nothing", fixture.wire.uploads-uploads, fixture.wire.deletedIDs)
	}
	if sess.SyncState != storage.SyncStateRemoteDeleted || !sess.DriveReplacePending {
		t.Errorf("with its folder in the trash: state=%q pending=%v, want %q with the replacement still owed", sess.SyncState, sess.DriveReplacePending, storage.SyncStateRemoteDeleted)
	}

	fixture.wire.mu.Lock()
	delete(fixture.wire.trashed, fixture.folder)
	fixture.wire.mu.Unlock()
	sweepOnce(t, fixture.store, fixture.orchestrator)
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("once the folder was restored, the sweep deleted %v, want the old ids %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
}

// TestASegmentDeletionInARemoteDeletedSessionIsReplacedOnceTheFolderIsBack
// fails if a segment deletion takes a session whose Drive folder is in the
// trash out of REMOTE_DELETED (the status warning stops counting it as kept
// on this unit only), or if the Drive replacement then never reaches it: the
// sweep must wait while the folder is in the trash and replace the folder's
// old files once it is back.
func TestASegmentDeletionInARemoteDeletedSessionIsReplacedOnceTheFolderIsBack(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.wire.allowDeletes = true
	fixture.wire.trash(fixture.folder)
	if marked, err := fixture.store.MarkRemoteDeleted(fixture.id, fixture.folder, "the folder is in the trash"); err != nil || !marked {
		t.Fatalf("mark remote deleted: %v, %v", marked, err)
	}
	segments, err := fixture.store.GetSegments(fixture.id)
	if err != nil || len(segments) == 0 {
		t.Fatalf("segments: %v, %v", segments, err)
	}
	before, err := fileSHA256(fixture.recording)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.recording+".spliced", []byte("recording without the deleted phrase"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := fileSHA256(fixture.recording + ".spliced")
	if err != nil {
		t.Fatal(err)
	}
	span := storage.DeletedSpan{SessionID: fixture.id, AudioPath: fixture.recording, Start: 0, End: 1, SHA256Before: before, SHA256After: after}
	if err := deleteRefinedAudioForTest(t, fixture.store, fixture.id, segments[0].ID, span); err != nil {
		t.Fatalf("delete refined segment: %v", err)
	}
	if err := os.Rename(fixture.recording+".spliced", fixture.recording); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.MarkSpliceComplete(&span); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ? WHERE id = ?`, storage.RefinementCompleted, storage.SummaryCompleted, fixture.id); err != nil {
		t.Fatal(err)
	}
	sess, err := fixture.store.GetSession(fixture.id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.SyncState != storage.SyncStateRemoteDeleted || !sess.DriveReplacePending {
		t.Fatalf("after the deletion: state=%q pending=%v, want %q with the replacement owed", sess.SyncState, sess.DriveReplacePending, storage.SyncStateRemoteDeleted)
	}
	if n, err := fixture.store.CountUnitOnlySessions(); err != nil || n != 1 {
		t.Errorf("after the deletion: CountUnitOnlySessions = %d, %v; want 1", n, err)
	}

	uploads := fixture.wire.uploads
	sweepOnce(t, fixture.store, fixture.orchestrator)
	if sess, err = fixture.store.GetSession(fixture.id); err != nil {
		t.Fatal(err)
	}
	if fixture.wire.uploads != uploads || len(fixture.wire.deletedIDs) != 0 || sess.SyncState != storage.SyncStateRemoteDeleted || !sess.DriveReplacePending {
		t.Errorf("with the folder in the trash: uploaded %d, deleted %v, state=%q pending=%v; want nothing touched, %q, still owed",
			fixture.wire.uploads-uploads, fixture.wire.deletedIDs, sess.SyncState, sess.DriveReplacePending, storage.SyncStateRemoteDeleted)
	}

	fixture.wire.mu.Lock()
	delete(fixture.wire.trashed, fixture.folder)
	fixture.wire.mu.Unlock()
	sweepOnce(t, fixture.store, fixture.orchestrator)
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("once the folder was back, the sweep deleted %v, want the old ids %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
	if sess, err = fixture.store.GetSession(fixture.id); err != nil {
		t.Fatal(err)
	}
	if sess.DriveReplacePending || sess.SyncState != storage.SyncStatePending {
		t.Errorf("after the replacement: state=%q pending=%v, want %q with nothing owed", sess.SyncState, sess.DriveReplacePending, storage.SyncStatePending)
	}
}

// TestReplacementOfAFolderGoneForGoodOwesNothing fails if a session whose
// Drive folder was deleted for good (and its old files with it) keeps owing
// a replacement, which no sweep could ever finish, or is not marked
// REMOTE_DELETED, as a sync marks it.
func TestReplacementOfAFolderGoneForGoodOwesNothing(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	fixture.wire.deleteByHand(fixture.folder)
	uploads := fixture.wire.uploads

	_ = fixture.orchestrator.SyncSession(context.Background(), fixture.id)
	sess, err := fixture.store.GetSession(fixture.id)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.wire.uploads != uploads {
		t.Errorf("the replacement uploaded %d files with its folder gone, want none", fixture.wire.uploads-uploads)
	}
	if sess.DriveReplacePending || sess.SyncState != storage.SyncStateRemoteDeleted {
		t.Errorf("with its folder gone for good: pending=%v state=%q, want nothing owed and %q", sess.DriveReplacePending, sess.SyncState, storage.SyncStateRemoteDeleted)
	}
}

// TestAFailingReplacementRecordsItsFailures fails if replacement attempts that
// keep failing are not counted in the store, with the last error, which the
// app's status warning is read from, or if the session is still listed as
// owing a replacement once it finishes.
func TestAFailingReplacementRecordsItsFailures(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	fixture.wire.allowDeletes = true
	fixture.wire.unavailable = true
	for attempt := 1; attempt <= 3; attempt++ {
		if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err == nil {
			t.Fatalf("attempt %d succeeded while Drive was unavailable", attempt)
		}
	}
	owed, err := fixture.store.OwedDriveReplacements()
	if err != nil {
		t.Fatal(err)
	}
	if len(owed) != 1 || owed[0].SessionID != fixture.id || owed[0].Failures != 3 || !strings.Contains(owed[0].LastError, "503") {
		t.Fatalf("owed replacements after three failed attempts = %+v, want %s with 3 failures and the last error", owed, fixture.id)
	}

	fixture.wire.unavailable = false
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("replacement once Drive was back: %v", err)
	}
	if owed, err = fixture.store.OwedDriveReplacements(); err != nil || len(owed) != 0 {
		t.Errorf("owed replacements after the replacement finished = %+v, %v; want none", owed, err)
	}
}

// TestAFailedReplacementNeverExhaustsItsRetries fails if a replacement that
// fails with the session's retries already used up leaves the session FAILED,
// or anywhere the sweep does not read.
func TestAFailedReplacementNeverExhaustsItsRetries(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	if _, err := fixture.store.DB().Exec(`UPDATE sessions SET retry_count = ? WHERE id = ?`, MaxSyncRetries, fixture.id); err != nil {
		t.Fatal(err)
	}
	// Drive fails the first upload (after the folder check passes).
	fixture.wire.failUploadAt = fixture.wire.uploads + 1
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err == nil {
		t.Fatal("the replacement succeeded though its first upload failed")
	}
	sess, err := fixture.store.GetSession(fixture.id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.SyncState != storage.SyncStatePending || sess.RetryCount != MaxSyncRetries || !sess.DriveReplacePending {
		t.Errorf("after a failed replacement with retries used up: state=%q retry=%d pending=%v, want %q, retry %d, still owed",
			sess.SyncState, sess.RetryCount, sess.DriveReplacePending, storage.SyncStatePending, MaxSyncRetries)
	}
	fixture.wire.allowDeletes = true
	sweepOnce(t, fixture.store, fixture.orchestrator)
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("the next sweep deleted %v, want the old ids %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
}
