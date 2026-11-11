package storage

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// summarizedSyncedSession creates session id with a summary, published to
// Envoy and synced to Drive.
func summarizedSyncedSession(t *testing.T, store *SQLiteStore, id string) {
	t.Helper()
	started := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(id, "Launch planning", "The last good summary.", SummaryCompleted, "default", `{"0":{"name":"Sam"}}`); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if err := setEnvoyPublishStatus(store, id, EnvoyPublishPublished); err != nil {
		t.Fatalf("published: %v", err)
	}
	syncTo(t, store, id, SyncStateSynced, "folder-1", 0, "")
}

// keepsLastGoodSummary fails the test unless sess holds the summary
// summarizedSyncedSession made, and is still synced.
func keepsLastGoodSummary(t *testing.T, sess *Session) {
	t.Helper()
	if sess.Title != "Launch planning" || sess.Summary != "The last good summary." ||
		sess.SummaryPreset != "default" || sess.SpeakerNames != `{"0":{"name":"Sam"}}` {
		t.Errorf("title %q, summary %q, preset %q, speakers %s; want the last good ones kept",
			sess.Title, sess.Summary, sess.SummaryPreset, sess.SpeakerNames)
	}
	if sess.SyncState != SyncStateSynced {
		t.Errorf("sync state %q, want %q: the documents did not change", sess.SyncState, SyncStateSynced)
	}
}

// While a summary is made, the session keeps the one it has: a summary that
// never ends (the app stops) takes nothing away. The new one is not yet
// published to Envoy.
func TestStartingASummaryKeepsTheLastOne(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106090000"
	summarizedSyncedSession(t, store, id)

	if err := store.StartSummary(id); err != nil {
		t.Fatalf("start summary: %v", err)
	}

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	keepsLastGoodSummary(t, &sess)
	if sess.SummaryStatus != SummaryRunning || sess.EnvoyPublishStatus != EnvoyPublishPending {
		t.Errorf("summary_status %q, envoy_publish_status %q; want %q, %q", sess.SummaryStatus, sess.EnvoyPublishStatus, SummaryRunning, EnvoyPublishPending)
	}
}

// A summary that fails takes nothing away: the session keeps its title and
// its last good summary, and records the failure as its summary's status and
// why. FailSummary returns the session as it then is.
func TestAFailedSummaryKeepsTheLastGoodOneAndRecordsWhy(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "20261106090000"
	summarizedSyncedSession(t, store, id)
	if err := store.StartSummary(id); err != nil {
		t.Fatalf("start summary: %v", err)
	}

	kept, err := store.FailSummary(id, "summarization failed: model unavailable")
	if err != nil {
		t.Fatalf("fail summary: %v", err)
	}

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	keepsLastGoodSummary(t, &sess)
	if sess.SummaryStatus != SummaryFailed || sess.SummaryError != "summarization failed: model unavailable" {
		t.Errorf("summary_status %q, summary_error %q; want %q and why", sess.SummaryStatus, sess.SummaryError, SummaryFailed)
	}
	if kept.Title != sess.Title || kept.Summary != sess.Summary || kept.SummaryPreset != sess.SummaryPreset || kept.SummaryStatus != SummaryFailed {
		t.Errorf("FailSummary returned title %q, summary %q, preset %q, status %q; want the session as it is",
			kept.Title, kept.Summary, kept.SummaryPreset, kept.SummaryStatus)
	}
}

func TestStartAndFailSummaryOfAMissingSession(t *testing.T) {
	store := newTestSQLiteStore(t)
	if err := store.StartSummary("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("start summary: %v, want %v", err, sql.ErrNoRows)
	}
	if _, err := store.FailSummary("missing", "why"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("fail summary: %v, want %v", err, sql.ErrNoRows)
	}
}
