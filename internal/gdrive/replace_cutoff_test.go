package gdrive

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/stopping"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestAReplacementTheStopCutsOffIsNotCountedFailed fails if a Drive
// replacement cut off by the app's stop during its upload (its context
// cancelled with stopping.ErrStopped) counts a failed replacement attempt, or
// ends its sync claim, instead of leaving the claim for the next start, whose
// recovery puts the session back in the state its claim came from. The
// control, a replacement whose context is cancelled for another reason,
// still counts the failure.
func TestAReplacementTheStopCutsOffIsNotCountedFailed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cause        error
		wantFailures int
	}{
		{"cut off by the stop", stopping.ErrStopped, 0},
		{"cancelled for another reason (control)", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplacementFixture(t)
			f.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			uploads := 0
			f.wire.mu.Lock()
			f.wire.onUpload = func(string) {
				if uploads++; uploads == 1 {
					cancel(tc.cause)
				}
			}
			f.wire.mu.Unlock()

			if err := f.orchestrator.SyncSession(ctx, f.id); err == nil {
				t.Fatal("control: the replacement cancelled during its upload succeeded")
			}
			if uploads == 0 {
				t.Fatal("control: the replacement uploaded nothing")
			}
			var failures int
			var state string
			if err := f.store.DB().QueryRow(`SELECT drive_replace_failures, sync_state FROM sessions WHERE id = ?`, f.id).Scan(&failures, &state); err != nil {
				t.Fatal(err)
			}
			if failures != tc.wantFailures {
				t.Fatalf("%d failed replacement attempts counted, want %d", failures, tc.wantFailures)
			}
			if tc.cause == nil {
				return
			}
			if state != storage.SyncStateSyncing {
				t.Fatalf("the replacement the stop cut off left the session %q, want it still %q for the next start", state, storage.SyncStateSyncing)
			}
			if _, err := f.store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			got, err := f.store.GetSession(f.id)
			if err != nil {
				t.Fatal(err)
			}
			if got.SyncState != storage.SyncStatePending || !got.DriveReplacePending {
				t.Fatalf("after the next start's recovery: %s, replacement owed %t; want %s and still owed", got.SyncState, got.DriveReplacePending, storage.SyncStatePending)
			}
		})
	}
}
