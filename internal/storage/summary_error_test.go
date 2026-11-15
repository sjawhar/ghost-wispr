package storage

import "testing"

// A summary that comes back empty (completed with nothing) never replaces the
// summary a session has: its summary, preset and speaker names stay.
func TestAnEmptyCompletedSummaryKeepsTheOneTheSessionHas(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106090000"
	summarizedSyncedSession(t, store, id)

	if err := store.UpdateSummaryAtGeneration(id, "", "", SummaryCompleted, "", "{}", "", 0); err != nil {
		t.Fatalf("store empty summary: %v", err)
	}

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	keepsLastGoodSummary(t, &sess)
	if sess.SummaryStatus != SummaryCompleted {
		t.Errorf("summary_status %q, want %q", sess.SummaryStatus, SummaryCompleted)
	}
}

// A failed summary records why in its own place: the session's sync error
// stays as it was.
func TestASummaryFailureLeavesTheSyncErrorAlone(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106090000"
	summarizedSyncedSession(t, store, id)
	if err := store.UpdateTitle(id, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	syncTo(t, store, id, SyncStateRetryScheduled, "folder-1", 1, "upload: Drive answered 503")

	if _, err := store.FailSummaryAtGeneration(id, "summarization failed: model unavailable", 0); err != nil {
		t.Fatalf("fail summary: %v", err)
	}

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.ErrorMessage != "upload: Drive answered 503" {
		t.Errorf("error_message %q, want the sync error kept", sess.ErrorMessage)
	}
	if sess.SummaryError != "summarization failed: model unavailable" {
		t.Errorf("summary_error %q, want why the summary failed", sess.SummaryError)
	}

	// A summary that completes clears the failure of the attempt before.
	if err := store.UpdateSummaryAtGeneration(id, "Launch planning", "A new summary.", SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatalf("store summary: %v", err)
	}
	if sess, err = store.GetSession(id); err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.SummaryError != "" || sess.ErrorMessage != "upload: Drive answered 503" {
		t.Errorf("after a completed summary: summary_error %q, error_message %q; want none and the sync error", sess.SummaryError, sess.ErrorMessage)
	}
}
