package gdrive

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/stopping"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestASyncTheStopCutsOffIsLeftForTheNextStart fails if a retry of a
// REMOTE_DELETED session whose folder is back, cut off by the app's stop
// during its upload (its context cancelled with stopping.ErrStopped), ends
// the sync as a failed one (RETRY_SCHEDULED, one retry more) instead of
// leaving it SYNCING for the next start, whose recovery puts it back to
// REMOTE_DELETED with its retry count as it was. The control, a sync whose
// context is cancelled for another reason, still ends as a failed sync.
func TestASyncTheStopCutsOffIsLeftForTheNextStart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cause       error
		wantState   string
		wantRetries int
	}{
		{"cut off by the stop", stopping.ErrStopped, storage.SyncStateRemoteDeleted, 4},
		{"cancelled for another reason (control)", nil, storage.SyncStateRetryScheduled, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, syncer := newWireDrive(t)
			store := openStore(t)
			o := NewOrchestrator(syncer, store)
			id, folder, _ := remoteDeletedSession(t, wire, store, o)
			if _, err := store.DB().Exec(`UPDATE sessions SET retry_count = 4 WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			asked := 0
			wire.mu.Lock()
			delete(wire.trashed, folder)
			// The first question about the folder is the retry's, before it
			// holds the session; the second is the upload's, after it does.
			wire.onGet = func(got string) {
				if got != folder {
					return
				}
				if asked++; asked == 2 {
					cancel(tc.cause)
				}
			}
			wire.mu.Unlock()

			if err := o.SyncSession(ctx, id); err == nil {
				t.Fatal("control: the sync cancelled during its upload succeeded")
			}
			if asked < 2 {
				t.Fatalf("control: Drive was asked about the folder %d times, want the retry's and the upload's", asked)
			}
			if tc.cause != nil {
				if state, _ := sessionState(t, store, id); state != storage.SyncStateSyncing {
					t.Fatalf("the sync the stop cut off left the session %q, want it still %q for the next start", state, storage.SyncStateSyncing)
				}
				if _, err := store.RecoverInterruptedSyncs(MaxSyncRetries, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := store.GetSession(id)
			if err != nil {
				t.Fatal(err)
			}
			if got.SyncState != tc.wantState || got.RetryCount != tc.wantRetries {
				t.Fatalf("session %s, %d retries; want %s, %d", got.SyncState, got.RetryCount, tc.wantState, tc.wantRetries)
			}
		})
	}
}
