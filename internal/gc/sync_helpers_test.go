package gc

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// syncClaims names each test claim apart, so two sessions claimed at once
// never share one claim.
var syncClaims atomic.Int64

// syncTo moves session id to sync state state the way a sync does: it claims
// the session, then marks it synced into folder or ends it in state with
// folder, retryCount and errorMessage. SYNCING leaves it claimed.
func syncTo(t *testing.T, store *storage.SQLiteStore, id, state, folder string, retryCount int, errorMessage string) {
	t.Helper()
	at := time.Now().UTC()
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession %s: %v", id, err)
	}
	claim := "test-claim-" + strconv.FormatInt(syncClaims.Add(1), 10)
	if _, ok, err := store.ClaimSync(id, session.SyncState, claim, at); err != nil || !ok {
		t.Fatalf("ClaimSync %s from %q = %v, %v; want claimed", id, session.SyncState, ok, err)
	}
	switch state {
	case storage.SyncStateSyncing:
	case storage.SyncStateSynced:
		if ok, err := store.MarkSynced(id, claim, folder, func(storage.Session, []transcribe.Segment) bool { return true }); err != nil || !ok {
			t.Fatalf("MarkSynced %s = %v, %v; want marked", id, ok, err)
		}
	default:
		if ok, err := store.EndSync(id, claim, state, folder, retryCount, at, errorMessage); err != nil || !ok {
			t.Fatalf("EndSync %s in %q = %v, %v; want ended", id, state, ok, err)
		}
	}
}
