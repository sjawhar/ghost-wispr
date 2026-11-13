package storage

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// Review of #38, round 1, S1: the documents trigger was made only if it did
// not exist, so a database that held an earlier body of it kept that body for
// good. Opening the store makes it again.
func TestOpeningTheStoreReplacesAnEarlierDocumentsTrigger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := newStoreAt(t, path)
	for _, stmt := range []string{
		`DROP TRIGGER sessions_documents_changed`,
		`CREATE TRIGGER sessions_documents_changed AFTER UPDATE OF title ON sessions BEGIN SELECT 1; END`,
	} {
		if _, err := store.DB().Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = newStoreAt(t, path)
	var body string
	if err := store.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'sessions_documents_changed'`).Scan(&body); err != nil {
		t.Fatalf("read trigger: %v", err)
	}
	if !strings.Contains(body, "canonical_transcript") || !strings.Contains(body, "sync_state = 'PENDING'") {
		t.Errorf("after reopening, the documents trigger is still the earlier one:\n%s", body)
	}
}

// A session whose documents are not verified on Drive (synced by an earlier
// version, see the mark-synced-documents-unverified migration) is never
// deleted by the age policy. Its audio is on Drive, so disk pressure may
// remove it after GC asks Drive first that the folder is there. A sync of it
// that completes verifies it, and the store counts the sessions still
// unverified.
func TestUnverifiedDocumentsHoldOffTheAgePolicy(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id, audio = "20261001100000", "/audio/20261001100000.mp3"
	started := time.Now().UTC().AddDate(0, 0, -40)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), audio); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := store.UpdateSummaryAtGeneration(id, "Standup", "A summary.", SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatalf("UpdateSummaryAtGeneration: %v", err)
	}
	sync := func(claim string) {
		t.Helper()
		sess, err := store.GetSession(id)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if _, ok, err := store.ClaimSync(id, sess.SyncState, claim, time.Now().UTC()); err != nil || !ok {
			t.Fatalf("ClaimSync = %v, %v", ok, err)
		}
		if ok, err := store.MarkSynced(id, claim, "folder-1", func(Session, []transcribe.Segment) bool { return true }); err != nil || !ok {
			t.Fatalf("MarkSynced = %v, %v", ok, err)
		}
	}
	sync("sync-1")
	if _, err := store.DB().Exec(`UPDATE sessions SET documents_unverified = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("mark unverified: %v", err)
	}

	if ids, err := store.GetGCEligibleSessions(30); err != nil || len(ids) != 0 {
		t.Errorf("GetGCEligibleSessions = %v, %v; want none", ids, err)
	}
	if deleted, err := deleteEndedSessionForTest(store, id, audio); err != nil || deleted {
		t.Errorf("DeleteEndedSession = %v, %v; want kept", deleted, err)
	}
	recordings, err := store.Recordings()
	if err != nil {
		t.Fatalf("Recordings: %v", err)
	}
	if len(recordings) != 1 || !recordings[0].Removable {
		t.Errorf("Recordings = %+v, want one removable recording", recordings)
	}
	if n, err := store.CountUnverifiedSessions(); err != nil || n != 1 {
		t.Errorf("CountUnverifiedSessions = %d, %v; want 1", n, err)
	}

	// Its title changes, and the sync of the change completes.
	if err := store.UpdateTitle(id, "Planning"); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}
	sync("sync-2")
	if ids, err := store.GetGCEligibleSessions(30); err != nil || !slices.Equal(ids, []string{id}) {
		t.Errorf("GetGCEligibleSessions after a completed sync = %v, %v; want [%s]", ids, err, id)
	}
	if n, err := store.CountUnverifiedSessions(); err != nil || n != 0 {
		t.Errorf("CountUnverifiedSessions after a completed sync = %d, %v; want 0", n, err)
	}
}
