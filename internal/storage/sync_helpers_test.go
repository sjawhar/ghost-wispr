package storage

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// syncClaims names each test claim apart, so two sessions claimed at once
// never share one claim.
var syncClaims atomic.Int64

// syncTo moves session id to sync state state the way a sync does: it claims
// the session, then marks it synced into folder or ends it in state with
// folder, retryCount and errorMessage. SYNCING leaves it claimed.
func syncTo(t *testing.T, store *SQLiteStore, id, state, folder string, retryCount int, errorMessage string) {
	t.Helper()
	syncToAt(t, store, id, state, folder, retryCount, errorMessage, time.Now().UTC())
}

// syncToAt is syncTo with the time the sync was tried, for a test that reads
// last_sync_attempt.
func syncToAt(t *testing.T, store *SQLiteStore, id, state, folder string, retryCount int, errorMessage string, at time.Time) {
	t.Helper()
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession %s: %v", id, err)
	}
	claim := "test-claim-" + strconv.FormatInt(syncClaims.Add(1), 10)
	if _, ok, err := store.ClaimSync(id, session.SyncState, claim, at); err != nil || !ok {
		t.Fatalf("ClaimSync %s from %q = %v, %v; want claimed", id, session.SyncState, ok, err)
	}
	switch state {
	case SyncStateSyncing:
	case SyncStateSynced:
		if ok, err := store.MarkSynced(id, claim, folder, unchangedSession); err != nil || !ok {
			t.Fatalf("MarkSynced %s = %v, %v; want marked", id, ok, err)
		}
	default:
		if ok, err := store.EndSync(id, claim, state, folder, retryCount, at, errorMessage); err != nil || !ok {
			t.Fatalf("EndSync %s in %q = %v, %v; want ended", id, state, ok, err)
		}
	}
}
