package storage

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	return store
}

func TestSQLitePragmas(t *testing.T) {
	store := newTestSQLiteStore(t)

	var mode string
	if err := store.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode failed: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected journal_mode wal, got %q", mode)
	}

	var timeout int
	if err := store.DB().QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout failed: %v", err)
	}
	if timeout < 5000 {
		t.Fatalf("expected busy_timeout >= 5000, got %d", timeout)
	}
}

func TestSQLiteCRUD(t *testing.T) {
	store := newTestSQLiteStore(t)

	startedAt := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	sessionID := startedAt.Format("20060102150405")
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	seg := transcribe.Segment{
		Speaker:   1,
		Text:      "Ship the polished app.",
		StartTime: 1.0,
		EndTime:   2.5,
		Timestamp: startedAt.Add(2 * time.Second),
	}
	if err := store.AppendSegment(sessionID, seg); err != nil {
		t.Fatalf("AppendSegment failed: %v", err)
	}

	if err := store.UpdateSummary(sessionID, "Meeting Notes", "## Summary\n- done", SummaryCompleted, "default"); err != nil {
		t.Fatalf("UpdateSummary failed: %v", err)
	}

	if err := store.EndSession(sessionID, startedAt.Add(30*time.Second), "data/audio/20260226100000.mp3"); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	session, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if session.Status != "ended" {
		t.Fatalf("expected status ended, got %q", session.Status)
	}
	if session.SummaryStatus != SummaryCompleted {
		t.Fatalf("expected summary_status %q, got %q", SummaryCompleted, session.SummaryStatus)
	}
	if session.SummaryPreset != "default" {
		t.Fatalf("expected summary_preset %q, got %q", "default", session.SummaryPreset)
	}

	segments, err := store.GetSegments(sessionID)
	if err != nil {
		t.Fatalf("GetSegments failed: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	if segments[0].Text != seg.Text {
		t.Fatalf("expected segment text %q, got %q", seg.Text, segments[0].Text)
	}

	sessionsByDate, err := store.GetSessionsByDate("2026-02-26", false)
	if err != nil {
		t.Fatalf("GetSessionsByDate failed: %v", err)
	}
	if len(sessionsByDate) != 1 {
		t.Fatalf("expected 1 session for date, got %d", len(sessionsByDate))
	}

	dates, err := store.GetDates()
	if err != nil {
		t.Fatalf("GetDates failed: %v", err)
	}
	if len(dates) != 1 || dates[0] != "2026-02-26" {
		t.Fatalf("expected dates [2026-02-26], got %#v", dates)
	}
}

func TestUpdateSummaryWithPreset(t *testing.T) {
	store := newTestSQLiteStore(t)

	startedAt := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	sessionID := startedAt.Format("20060102150405")
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	if err := store.UpdateSummary(sessionID, "Concise Notes", "## Summary\n- done", SummaryCompleted, "concise"); err != nil {
		t.Fatalf("UpdateSummary failed: %v", err)
	}

	session, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if session.SummaryPreset != "concise" {
		t.Fatalf("expected summary_preset %q, got %q", "concise", session.SummaryPreset)
	}
}

func TestBatchRefinement_StoreStatusAndTranscript(t *testing.T) {
	store := newTestSQLiteStore(t)

	startedAt := time.Date(2026, 3, 23, 9, 0, 0, 0, time.UTC)
	sessionID := startedAt.Format("20060102150405")
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	session, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if session.RefinementStatus != "pending" {
		t.Fatalf("expected default refinement_status pending, got %q", session.RefinementStatus)
	}

	if err := store.UpdateRefinement(sessionID, "", "running"); err != nil {
		t.Fatalf("UpdateRefinement running failed: %v", err)
	}

	if err := store.UpdateRefinement(sessionID, "refined canonical transcript", "completed"); err != nil {
		t.Fatalf("UpdateRefinement completed failed: %v", err)
	}

	session, err = store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession after refinement failed: %v", err)
	}
	if session.RefinementStatus != "completed" {
		t.Fatalf("expected refinement_status completed, got %q", session.RefinementStatus)
	}
	if session.RefinedTranscript != "refined canonical transcript" {
		t.Fatalf("expected refined transcript to persist, got %q", session.RefinedTranscript)
	}
}

func TestSQLiteSummaryClaimIsIdempotent(t *testing.T) {
	store := newTestSQLiteStore(t)

	claimed, err := store.ClaimSummaryRequest("s1", "hash-1")
	if err != nil {
		t.Fatalf("first claim failed: %v", err)
	}
	if !claimed {
		t.Fatal("expected first claim to be accepted")
	}

	claimed, err = store.ClaimSummaryRequest("s1", "hash-1")
	if err != nil {
		t.Fatalf("second claim failed: %v", err)
	}
	if claimed {
		t.Fatal("expected second claim to be ignored")
	}
}

func TestSyncStatusTracking(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "sync-test-1"
	started := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(30*time.Second), "data/audio/sync-test-1.mp3"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(sessionID, "Test Title", "Test summary", SummaryCompleted, "default"); err != nil {
		t.Fatalf("update summary: %v", err)
	}

	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatalf("get sessions needing sync: %v", err)
	}
	if len(ids) != 1 || ids[0] != sessionID {
		t.Fatalf("expected [%s], got %v", sessionID, ids)
	}

	if err := store.UpdateSyncStatus(sessionID, SyncSynced, "drive-folder-id-123"); err != nil {
		t.Fatalf("update sync status: %v", err)
	}

	ids, err = store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatalf("get sessions needing sync after update: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected empty, got %v", ids)
	}

	sess, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.SyncStatus != SyncSynced {
		t.Fatalf("expected sync_status %q, got %q", SyncSynced, sess.SyncStatus)
	}
	if sess.GDriveFolderID != "drive-folder-id-123" {
		t.Fatalf("expected gdrive_folder_id %q, got %q", "drive-folder-id-123", sess.GDriveFolderID)
	}
}

func TestGetGCEligibleSessions(t *testing.T) {
	store := newTestSQLiteStore(t)
	now := time.Now().UTC()
	old := now.Add(-45 * 24 * time.Hour)

	if err := store.CreateSession("gc-eligible", old); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gc-eligible", old.Add(time.Minute), "data/audio/gc-eligible.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSyncStatus("gc-eligible", SyncSynced, "folder-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummary("gc-eligible", "", "test summary", "completed", "default"); err != nil {
		t.Fatal(err)
	}

	if err := store.CreateSession("gc-unsynced", old); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gc-unsynced", old.Add(time.Minute), "data/audio/gc-unsynced.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummary("gc-unsynced", "", "test summary", "completed", "default"); err != nil {
		t.Fatal(err)
	}

	if err := store.CreateSession("gc-recent", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gc-recent", now.Add(-30*time.Minute), "data/audio/gc-recent.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSyncStatus("gc-recent", SyncSynced, "folder-2"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummary("gc-recent", "", "test summary", "completed", "default"); err != nil {
		t.Fatal(err)
	}

	ids, err := store.GetGCEligibleSessions(30, true, false)
	if err != nil {
		t.Fatalf("get gc eligible: %v", err)
	}
	if len(ids) != 1 || ids[0] != "gc-eligible" {
		t.Fatalf("expected [gc-eligible], got %v", ids)
	}

	ids, err = store.GetGCEligibleSessions(30, false, false)
	if err != nil {
		t.Fatalf("get gc eligible no gate: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 sessions, got %d: %v", len(ids), ids)
	}

	ids, err = store.GetGCEligibleSessions(0, true, true)
	if err != nil {
		t.Fatalf("get gc eligible disk pressure: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 synced sessions, got %d: %v", len(ids), ids)
	}
}

func TestDeleteSession(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "delete-test-1"
	started := time.Now().UTC()

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(sessionID, started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}

	seg := transcribe.Segment{Speaker: 0, Text: "hello", StartTime: 0, EndTime: 1, Timestamp: started}
	if err := store.AppendSegment(sessionID, seg); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteSession(sessionID); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	_, err := store.GetSession(sessionID)
	if err == nil {
		t.Fatal("expected error getting deleted session")
	}

	segs, err := store.GetSegments(sessionID)
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	if len(segs) != 0 {
		t.Fatalf("expected 0 segments, got %d", len(segs))
	}

	err = store.DeleteSession("nonexistent")
	if err == nil {
		t.Fatal("expected error deleting nonexistent session")
	}
}

func TestSQLiteConcurrentAccess(t *testing.T) {
	store := newTestSQLiteStore(t)

	startedAt := time.Now().UTC()
	sessionID := startedAt.Format("20060102150405")
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = store.AppendSegment(sessionID, transcribe.Segment{
				Speaker:   idx % 3,
				Text:      fmt.Sprintf("segment-%d", idx),
				StartTime: float64(idx),
				EndTime:   float64(idx) + 0.5,
				Timestamp: startedAt.Add(time.Duration(idx) * time.Second),
			})
			_, _ = store.GetSession(sessionID)
		}(i)
	}
	wg.Wait()

	segments, err := store.GetSegments(sessionID)
	if err != nil {
		t.Fatalf("GetSegments failed: %v", err)
	}
	if len(segments) != 20 {
		t.Fatalf("expected 20 segments, got %d", len(segments))
	}
}

func TestMergeSessions(t *testing.T) {
	store := newTestSQLiteStore(t)

	startA := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	startB := startA.Add(2 * time.Minute)

	if err := store.CreateSession("source-a", startA); err != nil {
		t.Fatalf("CreateSession source-a failed: %v", err)
	}
	if err := store.CreateSession("source-b", startB); err != nil {
		t.Fatalf("CreateSession source-b failed: %v", err)
	}

	if err := store.AppendSegment("source-a", transcribe.Segment{Speaker: 0, Text: "A1", StartTime: 0, EndTime: 1, Timestamp: startA.Add(time.Second)}); err != nil {
		t.Fatalf("AppendSegment source-a failed: %v", err)
	}
	if err := store.AppendSegment("source-b", transcribe.Segment{Speaker: 1, Text: "B1", StartTime: 0, EndTime: 1, Timestamp: startB.Add(time.Second)}); err != nil {
		t.Fatalf("AppendSegment source-b failed: %v", err)
	}

	if err := store.EndSession("source-a", startA.Add(90*time.Second), "data/audio/a.mp3"); err != nil {
		t.Fatalf("EndSession source-a failed: %v", err)
	}
	if err := store.EndSession("source-b", startB.Add(90*time.Second), "data/audio/b.mp3"); err != nil {
		t.Fatalf("EndSession source-b failed: %v", err)
	}

	mergedStart := startA
	mergedEnd := startB.Add(90 * time.Second)
	if err := store.MergeSessions("merged-1", []string{"source-a", "source-b"}, mergedStart, mergedEnd); err != nil {
		t.Fatalf("MergeSessions failed: %v", err)
	}

	merged, err := store.GetSession("merged-1")
	if err != nil {
		t.Fatalf("GetSession merged failed: %v", err)
	}
	if merged.StartedAt.UTC() != mergedStart.UTC() {
		t.Fatalf("expected merged started_at %s, got %s", mergedStart.UTC(), merged.StartedAt.UTC())
	}
	if merged.EndedAt == nil || merged.EndedAt.UTC() != mergedEnd.UTC() {
		t.Fatalf("expected merged ended_at %s, got %v", mergedEnd.UTC(), merged.EndedAt)
	}

	mergedSegments, err := store.GetSegments("merged-1")
	if err != nil {
		t.Fatalf("GetSegments merged failed: %v", err)
	}
	if len(mergedSegments) != 2 {
		t.Fatalf("expected 2 merged segments, got %d", len(mergedSegments))
	}

	sourceA, err := store.GetSession("source-a")
	if err != nil {
		t.Fatalf("GetSession source-a failed: %v", err)
	}
	if sourceA.Status != "merged" {
		t.Fatalf("expected source-a status merged, got %q", sourceA.Status)
	}
	if sourceA.MergedInto != "merged-1" {
		t.Fatalf("expected source-a merged_into merged-1, got %q", sourceA.MergedInto)
	}

	sourceB, err := store.GetSession("source-b")
	if err != nil {
		t.Fatalf("GetSession source-b failed: %v", err)
	}
	if sourceB.Status != "merged" {
		t.Fatalf("expected source-b status merged, got %q", sourceB.Status)
	}
	if sourceB.MergedInto != "merged-1" {
		t.Fatalf("expected source-b merged_into merged-1, got %q", sourceB.MergedInto)
	}
}

func TestGetSessionsByDate_ExcludesMerged(t *testing.T) {
	store := newTestSQLiteStore(t)

	base := time.Date(2026, 3, 5, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("source-1", base); err != nil {
		t.Fatalf("CreateSession source-1 failed: %v", err)
	}
	if err := store.CreateSession("source-2", base.Add(30*time.Minute)); err != nil {
		t.Fatalf("CreateSession source-2 failed: %v", err)
	}
	if err := store.EndSession("source-1", base.Add(10*time.Minute), ""); err != nil {
		t.Fatalf("EndSession source-1 failed: %v", err)
	}
	if err := store.EndSession("source-2", base.Add(40*time.Minute), ""); err != nil {
		t.Fatalf("EndSession source-2 failed: %v", err)
	}

	if err := store.MergeSessions("merged-day", []string{"source-1", "source-2"}, base, base.Add(40*time.Minute)); err != nil {
		t.Fatalf("MergeSessions failed: %v", err)
	}

	activeDate, err := store.GetSessionsByDate("2026-03-05", false)
	if err != nil {
		t.Fatalf("GetSessionsByDate includeDiscarded=false failed: %v", err)
	}
	for _, sess := range activeDate {
		if sess.Status == "merged" {
			t.Fatalf("expected merged sessions excluded, got session %q with status merged", sess.ID)
		}
	}
}

func TestSyncStateMetadataAndRetryQuery(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "sync-state-retry-1"
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(5*time.Minute), "data/audio/sync-state-retry-1.mp3"); err != nil {
		t.Fatalf("end session: %v", err)
	}

	attempt := started.Add(10 * time.Minute)
	if err := store.UpdateSyncState(sessionID, SyncStateRetryScheduled, "", 2, &attempt, "temporary upload failure"); err != nil {
		t.Fatalf("update sync state: %v", err)
	}

	sess, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.SyncState != SyncStateRetryScheduled {
		t.Fatalf("expected sync_state %q, got %q", SyncStateRetryScheduled, sess.SyncState)
	}
	if sess.RetryCount != 2 {
		t.Fatalf("expected retry_count 2, got %d", sess.RetryCount)
	}
	if sess.LastSyncAttempt == nil || !sess.LastSyncAttempt.Equal(attempt) {
		t.Fatalf("expected last_sync_attempt %s, got %v", attempt, sess.LastSyncAttempt)
	}
	if sess.ErrorMessage != "temporary upload failure" {
		t.Fatalf("expected error_message %q, got %q", "temporary upload failure", sess.ErrorMessage)
	}

	retrySessions, err := store.GetSessionsBySyncState(SyncStateRetryScheduled)
	if err != nil {
		t.Fatalf("get retry scheduled sessions: %v", err)
	}
	if len(retrySessions) != 1 || retrySessions[0].ID != sessionID {
		t.Fatalf("expected retry sessions [%s], got %+v", sessionID, retrySessions)
	}
}

func TestRemoteDeletedIsSoftDelete(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "sync-state-remote-deleted-1"
	started := time.Date(2026, 3, 23, 11, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(4*time.Minute), "data/audio/sync-state-remote-deleted-1.mp3"); err != nil {
		t.Fatalf("end session: %v", err)
	}

	attempt := started.Add(5 * time.Minute)
	if err := store.UpdateSyncState(sessionID, SyncStateRemoteDeleted, "", 0, &attempt, "remote folder deleted"); err != nil {
		t.Fatalf("update sync state remote deleted: %v", err)
	}

	sess, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.SyncState != SyncStateRemoteDeleted {
		t.Fatalf("expected sync_state %q, got %q", SyncStateRemoteDeleted, sess.SyncState)
	}
	if sess.Status != "ended" {
		t.Fatalf("expected local session status ended, got %q", sess.Status)
	}
	if sess.AudioPath == "" {
		t.Fatal("expected local audio path preserved for soft-delete")
	}
}
