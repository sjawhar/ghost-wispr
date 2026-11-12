package storage

import (
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// documentedSession ends a new session id with a streamed transcript, its
// transcript canonicalized and summary as its summary, ready for its first
// sync.
func documentedSession(t *testing.T, store *SQLiteStore, id, summary string) {
	t.Helper()
	endedWith(t, store, id, "/audio/"+id+".mp3", RefinementPending)
	if err := store.AppendSegment(id, transcribe.Segment{Text: "said", EndTime: 1, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("AppendSegment %s: %v", id, err)
	}
	if _, _, err := store.Canonicalize(id); err != nil {
		t.Fatalf("Canonicalize %s: %v", id, err)
	}
	if err := store.UpdateSummary(id, "Standup", summary, SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary %s: %v", id, err)
	}
}

// syncedWith documents a new session id (documentedSession) and marks it
// synced to Drive folder folder.
func syncedWith(t *testing.T, store *SQLiteStore, id, summary, folder string) {
	t.Helper()
	documentedSession(t, store, id, summary)
	syncTo(t, store, id, SyncStateSynced, folder, 0, "")
}

// Review of #37, round 5, note 1 (on main too): a change to a synced
// session's summary or transcript left it synced, so the change never reached
// Drive and the age policy deleted the newest text. The write that changes
// what a sync uploads (its summary, title, preset, canonical transcript or
// times) now sends a synced session back to pending sync in the same
// statement, keeping its Drive folder, so the sweep updates its documents in
// place. A write that changes none of that leaves it synced.
func TestADocumentChangeSendsASyncedSessionBackToSync(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, store *SQLiteStore, id string) error
		resync bool
	}{
		{"summary made again", func(_ *testing.T, store *SQLiteStore, id string) error {
			return store.UpdateSummary(id, "Standup", "The summary, made again.", SummaryCompleted, "default", "{}")
		}, true},
		{"summary started again", func(_ *testing.T, store *SQLiteStore, id string) error {
			return store.StartSummary(id)
		}, false},
		{"summary made again failed", func(t *testing.T, store *SQLiteStore, id string) error {
			if err := store.StartSummary(id); err != nil {
				t.Fatalf("StartSummary: %v", err)
			}
			_, err := store.FailSummary(id, "summarization failed")
			return err
		}, false},
		{"title edited", func(_ *testing.T, store *SQLiteStore, id string) error {
			return store.UpdateTitle(id, "Planning")
		}, true},
		{"refinement retried", func(t *testing.T, store *SQLiteStore, id string) error {
			if err := store.UpdateRefinementAtGeneration(id, "The refined transcript.", RefinementCompleted, "", 0); err != nil {
				t.Fatalf("UpdateRefinementAtGeneration: %v", err)
			}
			_, _, err := store.Canonicalize(id)
			return err
		}, true},
		{"same summary stored again", func(_ *testing.T, store *SQLiteStore, id string) error {
			return store.UpdateSummary(id, "Standup", "The first summary.", SummaryCompleted, "default", "{}")
		}, false},
		{"refined, not yet canonicalized", func(_ *testing.T, store *SQLiteStore, id string) error {
			return store.UpdateRefinementAtGeneration(id, "The refined transcript.", RefinementCompleted, "", 0)
		}, false},
		{"published to Envoy", func(_ *testing.T, store *SQLiteStore, id string) error {
			return setEnvoyPublishStatus(store, id, EnvoyPublishPublished)
		}, false},
		// Every other input of the documents, written directly: the app
		// changes none of these on a synced session today.
		{"preset changed", func(_ *testing.T, store *SQLiteStore, id string) error {
			_, err := store.DB().Exec(`UPDATE sessions SET summary_preset = 'detailed' WHERE id = ?`, id)
			return err
		}, true},
		{"start time changed", func(_ *testing.T, store *SQLiteStore, id string) error {
			_, err := store.DB().Exec(`UPDATE sessions SET started_at = '2026-11-06T13:59:00Z' WHERE id = ?`, id)
			return err
		}, true},
		{"end time changed", func(_ *testing.T, store *SQLiteStore, id string) error {
			_, err := store.DB().Exec(`UPDATE sessions SET ended_at = '2026-11-06T15:00:00Z' WHERE id = ?`, id)
			return err
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestSQLiteStore(t)
			const id = "20261106140000"
			syncedWith(t, store, id, "The first summary.", "folder-1")
			if err := tc.change(t, store, id); err != nil {
				t.Fatalf("change: %v", err)
			}
			session, err := store.GetSession(id)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			wantState, wantStatus := SyncStateSynced, SyncSynced
			if tc.resync {
				wantState, wantStatus = SyncStatePending, SyncPending
			}
			if session.SyncState != wantState || session.SyncStatus != wantStatus || session.GDriveFolderID != "folder-1" {
				t.Errorf("after the change: %q/%q in %q, want %q/%q in folder-1", session.SyncState, session.SyncStatus, session.GDriveFolderID, wantState, wantStatus)
			}
			if pending, err := store.GetSessionsNeedingSync(); err != nil || (len(pending) == 1) != tc.resync {
				t.Errorf("sessions the sweep syncs: %v (%v), want the session listed: %v", pending, err, tc.resync)
			}
			// The search index still matches the sessions it indexes.
			if _, err := store.DB().Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('integrity-check')`); err != nil {
				t.Errorf("search index after the change: %v", err)
			}
		})
	}
}

// A session restored from Drive is synced (sync_status) with no sync state
// of its own; a change to its documents sends it to sync into the folder it
// was restored from.
func TestADocumentChangeSendsARestoredSessionBackToSync(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	if err := store.ImportSession(&Session{
		ID: "20260301100000", Title: "Standup", StartedAt: started, EndedAt: &ended, Status: SessionEnded,
		Summary: "The summary.", SummaryStatus: SummaryCompleted, SyncStatus: SyncSynced, GDriveFolderID: "folder-restored",
	}, []transcribe.Segment{{Text: "said", EndTime: 1, Timestamp: started}}); err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if session, _ := store.GetSession("20260301100000"); session.SyncStatus != SyncSynced {
		t.Fatalf("restored session: sync status %q, want %q", session.SyncStatus, SyncSynced)
	}
	if err := store.UpdateTitle("20260301100000", "Planning"); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}
	session, err := store.GetSession("20260301100000")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.SyncState != SyncStatePending || session.SyncStatus != SyncPending || session.GDriveFolderID != "folder-restored" {
		t.Errorf("after the title edit: %q/%q in %q, want %q/%q in folder-restored", session.SyncState, session.SyncStatus, session.GDriveFolderID, SyncStatePending, SyncPending)
	}
}

// A session not synced keeps its sync state when its documents change: a
// failed or pending sync uploads them as they are when it runs, and one whose
// Drive folder was deleted by hand is not synced again.
func TestADocumentChangeLeavesASessionNotSyncedAsItIs(t *testing.T) {
	for _, state := range []string{SyncStatePending, SyncStateRetryScheduled, SyncStateFailed, SyncStateRemoteDeleted} {
		t.Run(state, func(t *testing.T) {
			store := newTestSQLiteStore(t)
			const id = "20261106140000"
			documentedSession(t, store, id, "The first summary.")
			syncTo(t, store, id, state, "folder-1", 1, "an earlier sync")
			if err := store.UpdateTitle(id, "Planning"); err != nil {
				t.Fatalf("UpdateTitle: %v", err)
			}
			session, err := store.GetSession(id)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if session.SyncState != state || session.RetryCount != 1 || session.ErrorMessage != "an earlier sync" {
				t.Errorf("after the title edit: %q, retry %d, error %q; want %q left as it was", session.SyncState, session.RetryCount, session.ErrorMessage, state)
			}
		})
	}
}
