package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestPreMigrationBackupCreated(t *testing.T) {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")

	// Create store (which triggers backup creation)
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Verify backup file exists
	backupPath := dbPath + ".pre-migrate.bak"
	_, err = os.Stat(backupPath)
	if err != nil {
		t.Fatalf("backup file not found at %s: %v", backupPath, err)
	}

	// Verify backup has content (not empty)
	backupInfo, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if backupInfo.Size() == 0 {
		t.Fatal("backup file is empty")
	}

	// Verify backup is a valid SQLite database by opening it
	backupStore, err := NewSQLiteStore(backupPath)
	if err != nil {
		t.Fatalf("backup is not a valid SQLite database: %v", err)
	}
	defer func() { _ = backupStore.Close() }()

	// Verify we can query the backup
	var count int
	if err := backupStore.DB().QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
		t.Fatalf("query backup failed: %v", err)
	}
}

func TestPreMigrationBackupCreatedEvenForFreshInstall(t *testing.T) {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "fresh.db")

	// Verify DB file doesn't exist yet
	_, err := os.Stat(dbPath)
	if err == nil {
		t.Fatal("DB file should not exist yet")
	}

	// Create store (which creates DB file via pragmas, then backs it up)
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Verify backup file IS created even for fresh install
	// (because pragmas create the DB file, then backup runs)
	backupPath := dbPath + ".pre-migrate.bak"
	_, err = os.Stat(backupPath)
	if err != nil {
		t.Fatalf("backup file should be created: %v", err)
	}

	// Verify backup is a valid SQLite database
	backupStore, err := NewSQLiteStore(backupPath)
	if err != nil {
		t.Fatalf("backup is not a valid SQLite database: %v", err)
	}
	_ = backupStore.Close()
}

func TestPreMigrationBackupOverwritesPrevious(t *testing.T) {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "overwrite.db")

	// Create first store and backup
	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("first NewSQLiteStore failed: %v", err)
	}
	backupPath := dbPath + ".pre-migrate.bak"

	// Get first backup's modification time
	backupInfo1, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("stat first backup: %v", err)
	}
	firstModTime := backupInfo1.ModTime()

	_ = store1.Close()

	// Wait a bit to ensure different modification time
	time.Sleep(10 * time.Millisecond)

	// Create second store (should overwrite backup)
	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("second NewSQLiteStore failed: %v", err)
	}
	defer func() { _ = store2.Close() }()

	// Get second backup's modification time
	backupInfo2, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("stat second backup: %v", err)
	}
	secondModTime := backupInfo2.ModTime()

	// Verify backup was overwritten (newer modification time)
	if !secondModTime.After(firstModTime) {
		t.Fatalf("backup was not overwritten: first=%v, second=%v", firstModTime, secondModTime)
	}

	// Verify backup is still valid
	backupStore, err := NewSQLiteStore(backupPath)
	if err != nil {
		t.Fatalf("backup is not a valid SQLite database: %v", err)
	}
	_ = backupStore.Close()
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

func TestCanonicalize_FromRefinedTranscript(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "canon-refined-1"
	started := time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(5*time.Minute), ""); err != nil {
		t.Fatalf("end session: %v", err)
	}

	// Add streaming segments.
	for _, text := range []string{"hello world", "how are you"} {
		if err := store.AppendSegment(sessionID, transcribe.Segment{Speaker: 0, Text: text, StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
			t.Fatalf("append segment: %v", err)
		}
	}

	// Set refined transcript.
	if err := store.UpdateRefinement(sessionID, "refined hello world. how are you doing?", "completed"); err != nil {
		t.Fatalf("update refinement: %v", err)
	}

	// Canonicalize should use refined transcript.
	if err := store.Canonicalize(sessionID); err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	transcript, source, err := store.GetCanonicalTranscript(sessionID)
	if err != nil {
		t.Fatalf("get canonical: %v", err)
	}
	if source != "refined" {
		t.Fatalf("expected transcript_source 'refined', got %q", source)
	}
	if transcript != "refined hello world. how are you doing?" {
		t.Fatalf("expected refined transcript, got %q", transcript)
	}

	// Verify session struct also has the fields.
	sess, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.TranscriptSource != "refined" {
		t.Fatalf("expected session.TranscriptSource 'refined', got %q", sess.TranscriptSource)
	}
	if sess.CanonicalTranscript != "refined hello world. how are you doing?" {
		t.Fatalf("expected session.CanonicalTranscript to match refined, got %q", sess.CanonicalTranscript)
	}
}

func TestCanonicalize_FallbackToStreaming(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "canon-streaming-1"
	started := time.Date(2026, 3, 23, 13, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(5*time.Minute), ""); err != nil {
		t.Fatalf("end session: %v", err)
	}

	// Add streaming segments.
	for _, text := range []string{"segment one", "segment two"} {
		if err := store.AppendSegment(sessionID, transcribe.Segment{Speaker: 0, Text: text, StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
			t.Fatalf("append segment: %v", err)
		}
	}

	// Refinement pending (default) — should fall back to streaming.
	if err := store.Canonicalize(sessionID); err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	transcript, source, err := store.GetCanonicalTranscript(sessionID)
	if err != nil {
		t.Fatalf("get canonical: %v", err)
	}
	if source != "streaming" {
		t.Fatalf("expected transcript_source 'streaming', got %q", source)
	}
	if transcript != "segment one\nsegment two\n" {
		t.Fatalf("expected assembled streaming transcript, got %q", transcript)
	}
}

func TestCanonicalize_RefinementFailed_FallsBackToStreaming(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "canon-failed-1"
	started := time.Date(2026, 3, 23, 14, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(5*time.Minute), ""); err != nil {
		t.Fatalf("end session: %v", err)
	}

	if err := store.AppendSegment(sessionID, transcribe.Segment{Speaker: 0, Text: "fallback text", StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
		t.Fatalf("append segment: %v", err)
	}

	// Mark refinement as failed.
	if err := store.UpdateRefinement(sessionID, "", "failed"); err != nil {
		t.Fatalf("update refinement: %v", err)
	}

	if err := store.Canonicalize(sessionID); err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	_, source, err := store.GetCanonicalTranscript(sessionID)
	if err != nil {
		t.Fatalf("get canonical: %v", err)
	}
	if source != "streaming" {
		t.Fatalf("expected transcript_source 'streaming' after failed refinement, got %q", source)
	}
}

func TestCanonicalize_RecanonicalizeAfterLateRefinement(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "canon-recanon-1"
	started := time.Date(2026, 3, 23, 15, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(5*time.Minute), ""); err != nil {
		t.Fatalf("end session: %v", err)
	}

	if err := store.AppendSegment(sessionID, transcribe.Segment{Speaker: 0, Text: "initial streaming", StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
		t.Fatalf("append segment: %v", err)
	}

	// First canonicalization — streaming (refinement pending).
	if err := store.Canonicalize(sessionID); err != nil {
		t.Fatalf("first canonicalize: %v", err)
	}
	_, source, _ := store.GetCanonicalTranscript(sessionID)
	if source != "streaming" {
		t.Fatalf("expected initial source 'streaming', got %q", source)
	}

	// Late refinement completes.
	if err := store.UpdateRefinement(sessionID, "late refined transcript", "completed"); err != nil {
		t.Fatalf("update refinement: %v", err)
	}

	// Re-canonicalize — should now use refined.
	if err := store.Canonicalize(sessionID); err != nil {
		t.Fatalf("re-canonicalize: %v", err)
	}

	transcript, source, err := store.GetCanonicalTranscript(sessionID)
	if err != nil {
		t.Fatalf("get canonical: %v", err)
	}
	if source != "refined" {
		t.Fatalf("expected re-canonicalized source 'refined', got %q", source)
	}
	if transcript != "late refined transcript" {
		t.Fatalf("expected re-canonicalized transcript, got %q", transcript)
	}
}

func TestFTS5IndexAndTriggersAreCreated(t *testing.T) {
	store := newTestSQLiteStore(t)

	var ftsSQL string
	if err := store.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'sessions_fts'`).Scan(&ftsSQL); err != nil {
		t.Fatalf("sessions_fts schema lookup failed: %v", err)
	}

	lowerSQL := strings.ToLower(ftsSQL)
	if !strings.Contains(lowerSQL, "using fts5") {
		t.Fatalf("expected sessions_fts to use fts5, got %q", ftsSQL)
	}
	if !strings.Contains(lowerSQL, "content='sessions'") {
		t.Fatalf("expected sessions_fts to be external-content table, got %q", ftsSQL)
	}

	for _, trigger := range []string{"sessions_ai", "sessions_au", "sessions_ad"} {
		var count int
		if err := store.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?`, trigger).Scan(&count); err != nil {
			t.Fatalf("trigger lookup %s failed: %v", trigger, err)
		}
		if count != 1 {
			t.Fatalf("expected trigger %s to exist", trigger)
		}
	}
}

func TestFTS5SearchReturnsRankedHighlightedResults(t *testing.T) {
	store := newTestSQLiteStore(t)

	started := time.Date(2026, 3, 23, 16, 0, 0, 0, time.UTC)
	if err := store.CreateSession("fts-1", started); err != nil {
		t.Fatalf("create session fts-1: %v", err)
	}
	if err := store.UpdateSummary("fts-1", "Alpha planning", "Discussed alpha rollout", SummaryCompleted, "default"); err != nil {
		t.Fatalf("update summary fts-1: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = ? WHERE id = ?`, "alpha appears once in transcript", "fts-1"); err != nil {
		t.Fatalf("update canonical transcript fts-1: %v", err)
	}

	if err := store.CreateSession("fts-2", started.Add(time.Minute)); err != nil {
		t.Fatalf("create session fts-2: %v", err)
	}
	if err := store.UpdateSummary("fts-2", "Alpha alpha alpha", "Alpha mentioned repeatedly", SummaryCompleted, "default"); err != nil {
		t.Fatalf("update summary fts-2: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = ? WHERE id = ?`, "alpha alpha alpha and alpha again", "fts-2"); err != nil {
		t.Fatalf("update canonical transcript fts-2: %v", err)
	}

	results, err := store.Search("alpha", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 search results, got %d", len(results))
	}

	if !strings.Contains(results[0].Snippet, "<mark>") {
		t.Fatalf("expected highlighted snippet, got %q", results[0].Snippet)
	}
	if results[0].SessionID == "" || results[0].Title == "" {
		t.Fatalf("expected search result identifiers, got %+v", results[0])
	}

	if results[0].Rank > results[1].Rank {
		t.Fatalf("expected first result to be equal/better rank ordering, got %f > %f", results[0].Rank, results[1].Rank)
	}
}

func TestFTS5TriggersStayInSyncOnUpdateAndDelete(t *testing.T) {
	store := newTestSQLiteStore(t)

	started := time.Date(2026, 3, 23, 17, 0, 0, 0, time.UTC)
	if err := store.CreateSession("fts-sync", started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.UpdateSummary("fts-sync", "Initial title", "Initial summary", SummaryCompleted, "default"); err != nil {
		t.Fatalf("update summary initial: %v", err)
	}

	results, err := store.Search("delta", SearchOptions{})
	if err != nil {
		t.Fatalf("search initial: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no matches before update, got %d", len(results))
	}

	if err := store.UpdateSummary("fts-sync", "Delta title", "Summary with delta keyword", SummaryCompleted, "default"); err != nil {
		t.Fatalf("update summary delta: %v", err)
	}

	results, err = store.Search("delta", SearchOptions{})
	if err != nil {
		t.Fatalf("search after update: %v", err)
	}
	if len(results) != 1 || results[0].SessionID != "fts-sync" {
		t.Fatalf("expected one updated match for fts-sync, got %+v", results)
	}

	if err := store.DeleteSession("fts-sync"); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	results, err = store.Search("delta", SearchOptions{})
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no matches after delete, got %+v", results)
	}
}

func TestFTS5SearchEmptyQueryReturnsEmptyResults(t *testing.T) {
	store := newTestSQLiteStore(t)

	results, err := store.Search("   ", SearchOptions{})
	if err != nil {
		t.Fatalf("search empty query failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty results for empty query, got %+v", results)
	}
}

func TestFTS5SearchEscapesSpecialCharacters(t *testing.T) {
	store := newTestSQLiteStore(t)

	started := time.Date(2026, 3, 23, 18, 0, 0, 0, time.UTC)
	if err := store.CreateSession("fts-escape", started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.UpdateSummary("fts-escape", "Alpha Operators", "Contains alpha keyword", SummaryCompleted, "default"); err != nil {
		t.Fatalf("update summary: %v", err)
	}

	results, err := store.Search(`alpha OR "`, SearchOptions{})
	if err != nil {
		t.Fatalf("search with special characters failed: %v", err)
	}
	if len(results) != 1 || results[0].SessionID != "fts-escape" {
		t.Fatalf("expected escaped query to return fts-escape, got %+v", results)
	}
}

func TestSearchWithDateFromFilter(t *testing.T) {
	store := newTestSQLiteStore(t)

	// Create sessions with different dates
	date1 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	date2 := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	date3 := time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC)

	for i, date := range []time.Time{date1, date2, date3} {
		id := fmt.Sprintf("date-test-%d", i)
		if err := store.CreateSession(id, date); err != nil {
			t.Fatalf("create session: %v", err)
		}
		if err := store.UpdateSummary(id, "Test", "test content", SummaryCompleted, "default"); err != nil {
			t.Fatalf("update summary: %v", err)
		}
	}

	// Search with date_from filter
	results, err := store.Search("test", SearchOptions{
		DateFrom: date2.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("search with date_from failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (date2 and date3), got %d", len(results))
	}
}

func TestSearchWithDateToFilter(t *testing.T) {
	store := newTestSQLiteStore(t)

	// Create sessions with different dates
	date1 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	date2 := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	date3 := time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC)

	for i, date := range []time.Time{date1, date2, date3} {
		id := fmt.Sprintf("date-to-test-%d", i)
		if err := store.CreateSession(id, date); err != nil {
			t.Fatalf("create session: %v", err)
		}
		if err := store.UpdateSummary(id, "Test", "test content", SummaryCompleted, "default"); err != nil {
			t.Fatalf("update summary: %v", err)
		}
	}

	// Search with date_to filter
	results, err := store.Search("test", SearchOptions{
		DateTo: date2.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("search with date_to failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (date1 and date2), got %d", len(results))
	}
}

func TestSearchWithPresetFilter(t *testing.T) {
	store := newTestSQLiteStore(t)

	// Create sessions with different presets
	presets := []string{"default", "meeting", "default"}
	for i, preset := range presets {
		id := fmt.Sprintf("preset-test-%d", i)
		date := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
		if err := store.CreateSession(id, date); err != nil {
			t.Fatalf("create session: %v", err)
		}
		if err := store.UpdateSummary(id, "Test", "test content", SummaryCompleted, preset); err != nil {
			t.Fatalf("update summary: %v", err)
		}
	}

	// Search with preset filter
	results, err := store.Search("test", SearchOptions{
		Preset: "meeting",
	})
	if err != nil {
		t.Fatalf("search with preset failed: %v", err)
	}
	if len(results) != 1 || results[0].SessionID != "preset-test-1" {
		t.Fatalf("expected 1 result for preset 'meeting', got %+v", results)
	}
}

func TestSearchWithMultipleFilters(t *testing.T) {
	store := newTestSQLiteStore(t)

	// Create sessions with different dates and presets
	date1 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	date2 := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	date3 := time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC)

	for i, date := range []time.Time{date1, date2, date3} {
		id := fmt.Sprintf("multi-test-%d", i)
		preset := "default"
		if i == 1 {
			preset = "meeting"
		}
		if err := store.CreateSession(id, date); err != nil {
			t.Fatalf("create session: %v", err)
		}
		if err := store.UpdateSummary(id, "Test", "test content", SummaryCompleted, preset); err != nil {
			t.Fatalf("update summary: %v", err)
		}
	}

	// Search with both date and preset filters
	results, err := store.Search("test", SearchOptions{
		DateFrom: date2.Format(time.RFC3339Nano),
		DateTo:   date3.Format(time.RFC3339Nano),
		Preset:   "meeting",
	})
	if err != nil {
		t.Fatalf("search with multiple filters failed: %v", err)
	}
	if len(results) != 1 || results[0].SessionID != "multi-test-1" {
		t.Fatalf("expected 1 result matching all filters, got %+v", results)
	}
}

func TestSegmentsInTimeRange(t *testing.T) {
	store := newTestSQLiteStore(t)

	startedAt := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	sessionID := "time-range-test"
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if err := store.EndSession(sessionID, startedAt.Add(30*time.Minute), ""); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	// Insert segments at different times
	for i, seg := range []transcribe.Segment{
		{Speaker: 0, Text: "early segment", StartTime: 10.0, EndTime: 15.0, Timestamp: startedAt.Add(10 * time.Second)},
		{Speaker: 1, Text: "match segment", StartTime: 60.0, EndTime: 65.0, Timestamp: startedAt.Add(60 * time.Second)},
		{Speaker: 0, Text: "nearby segment", StartTime: 120.0, EndTime: 125.0, Timestamp: startedAt.Add(120 * time.Second)},
		{Speaker: 1, Text: "far segment", StartTime: 600.0, EndTime: 605.0, Timestamp: startedAt.Add(600 * time.Second)},
		{Speaker: 0, Text: "very far segment", StartTime: 1200.0, EndTime: 1205.0, Timestamp: startedAt.Add(1200 * time.Second)},
	} {
		if err := store.AppendSegment(sessionID, seg); err != nil {
			t.Fatalf("AppendSegment %d failed: %v", i, err)
		}
	}

	// Query a time range that should include segments 1-3 (60-600 range, using 0-200)
	segs, err := store.GetSegmentsInTimeRange(sessionID, 50.0, 200.0)
	if err != nil {
		t.Fatalf("GetSegmentsInTimeRange failed: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("expected 2 segments in range [50,200], got %d", len(segs))
	}
	if segs[0].Text != "match segment" {
		t.Fatalf("expected first segment 'match segment', got %q", segs[0].Text)
	}
	if segs[1].Text != "nearby segment" {
		t.Fatalf("expected second segment 'nearby segment', got %q", segs[1].Text)
	}

	// Query range with no matches
	segs, err = store.GetSegmentsInTimeRange(sessionID, 300.0, 500.0)
	if err != nil {
		t.Fatalf("GetSegmentsInTimeRange empty range failed: %v", err)
	}
	if len(segs) != 0 {
		t.Fatalf("expected 0 segments in range [300,500], got %d", len(segs))
	}

	// Query range covering all segments
	segs, err = store.GetSegmentsInTimeRange(sessionID, 0.0, 2000.0)
	if err != nil {
		t.Fatalf("GetSegmentsInTimeRange full range failed: %v", err)
	}
	if len(segs) != 5 {
		t.Fatalf("expected 5 segments in range [0,2000], got %d", len(segs))
	}

	// Verify segments are ordered by start_time
	for i := 1; i < len(segs); i++ {
		if segs[i].StartTime < segs[i-1].StartTime {
			t.Fatalf("segments not ordered by start_time: %f < %f", segs[i].StartTime, segs[i-1].StartTime)
		}
	}
}

func TestAggregateSessions(t *testing.T) {
	store := newTestSQLiteStore(t)

	// Create sessions across different dates and presets
	date1 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	date2 := time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC)
	date3 := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	date4 := time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC)

	sessions := []struct {
		id     string
		date   time.Time
		preset string
	}{
		{"agg-1", date1, "meeting"},
		{"agg-2", date2, "standup"},
		{"agg-3", date3, "meeting"},
		{"agg-4", date4, "standup"},
	}

	for _, s := range sessions {
		if err := store.CreateSession(s.id, s.date); err != nil {
			t.Fatalf("create session %s: %v", s.id, err)
		}
		if err := store.EndSession(s.id, s.date.Add(30*time.Minute), ""); err != nil {
			t.Fatalf("end session %s: %v", s.id, err)
		}
		if err := store.UpdateSummary(s.id, "Title "+s.id, "Summary", SummaryCompleted, s.preset); err != nil {
			t.Fatalf("update summary %s: %v", s.id, err)
		}
	}

	// Also create a discarded session to ensure it's excluded
	if err := store.CreateSession("agg-discarded", date1); err != nil {
		t.Fatalf("create discarded session: %v", err)
	}
	if err := store.DiscardSession("agg-discarded"); err != nil {
		t.Fatalf("discard session: %v", err)
	}

	t.Run("no filters group by date", func(t *testing.T) {
		result, err := store.AggregateSessions(AggregateOptions{})
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if result.SessionCount != 4 {
			t.Fatalf("expected 4 sessions, got %d", result.SessionCount)
		}
		if len(result.Groups) != 3 {
			t.Fatalf("expected 3 date groups, got %d", len(result.Groups))
		}
		// Sessions ordered DESC, so most recent date first
		if result.Groups[0].Key != "2026-03-30" {
			t.Fatalf("expected first group 2026-03-30, got %q", result.Groups[0].Key)
		}
		if result.Groups[1].Key != "2026-03-25" {
			t.Fatalf("expected second group 2026-03-25, got %q", result.Groups[1].Key)
		}
		if result.Groups[2].Key != "2026-03-20" {
			t.Fatalf("expected third group 2026-03-20, got %q", result.Groups[2].Key)
		}
		// Date 2026-03-20 has 2 sessions
		if result.Groups[2].Count != 2 {
			t.Fatalf("expected 2 sessions on 2026-03-20, got %d", result.Groups[2].Count)
		}
	})

	t.Run("group by preset", func(t *testing.T) {
		result, err := store.AggregateSessions(AggregateOptions{GroupBy: "preset"})
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if result.SessionCount != 4 {
			t.Fatalf("expected 4 sessions, got %d", result.SessionCount)
		}
		if len(result.Groups) != 2 {
			t.Fatalf("expected 2 preset groups, got %d", len(result.Groups))
		}
	})

	t.Run("filter by date range", func(t *testing.T) {
		result, err := store.AggregateSessions(AggregateOptions{
			DateFrom: date3.Format(time.RFC3339Nano),
			DateTo:   date4.Add(time.Hour).Format(time.RFC3339Nano),
		})
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if result.SessionCount != 2 {
			t.Fatalf("expected 2 sessions in date range, got %d", result.SessionCount)
		}
	})

	t.Run("filter by preset", func(t *testing.T) {
		result, err := store.AggregateSessions(AggregateOptions{Preset: "meeting"})
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if result.SessionCount != 2 {
			t.Fatalf("expected 2 meeting sessions, got %d", result.SessionCount)
		}
		for _, g := range result.Groups {
			for _, s := range g.Sessions {
				if s.SummaryPreset != "meeting" {
					t.Fatalf("expected all sessions to be meeting preset, got %q", s.SummaryPreset)
				}
			}
		}
	})

	t.Run("session summary fields populated", func(t *testing.T) {
		result, err := store.AggregateSessions(AggregateOptions{Preset: "standup", GroupBy: "date"})
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if result.SessionCount != 2 {
			t.Fatalf("expected 2 standup sessions, got %d", result.SessionCount)
		}
		s := result.Groups[0].Sessions[0]
		if s.ID == "" || s.Title == "" || s.StartedAt.IsZero() || s.SummaryPreset != "standup" {
			t.Fatalf("session summary missing fields: %+v", s)
		}
	})
}

func TestStoreEmbedding(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "embed-test-1"
	startedAt := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("create session: %v", err)
	}
	
	// Create test vector
	vector := []float32{0.1, 0.2, 0.3, 0.4, 0.5}
	textHash := "hash-abc123"
	model := "openai/text-embedding-3-small"
	
	// Store embedding
	if err := store.StoreEmbedding(sessionID, 0, vector, textHash, model); err != nil {
		t.Fatalf("store embedding: %v", err)
	}
	
	// Retrieve and verify
	embeddings, err := store.GetEmbeddings(sessionID)
	if err != nil {
		t.Fatalf("get embeddings: %v", err)
	}
	if len(embeddings) != 1 {
		t.Fatalf("expected 1 embedding, got %d", len(embeddings))
	}
	
	emb := embeddings[0]
	if emb.SessionID != sessionID {
		t.Fatalf("expected session_id %q, got %q", sessionID, emb.SessionID)
	}
	if emb.ChunkIndex != 0 {
		t.Fatalf("expected chunk_index 0, got %d", emb.ChunkIndex)
	}
	if emb.TextHash != textHash {
		t.Fatalf("expected text_hash %q, got %q", textHash, emb.TextHash)
	}
	if emb.Model != model {
		t.Fatalf("expected model %q, got %q", model, emb.Model)
	}
	if len(emb.Vector) != len(vector) {
		t.Fatalf("expected vector length %d, got %d", len(vector), len(emb.Vector))
	}
	for i, v := range vector {
		if emb.Vector[i] != v {
			t.Fatalf("vector[%d]: expected %f, got %f", i, v, emb.Vector[i])
		}
	}
	if emb.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
}

func TestDeleteEmbeddings(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "embed-delete-1"
	startedAt := time.Date(2026, 3, 25, 11, 0, 0, 0, time.UTC)
	
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("create session: %v", err)
	}
	
	// Store multiple embeddings
	for i := 0; i < 3; i++ {
		vector := []float32{float32(i) * 0.1, float32(i) * 0.2}
		if err := store.StoreEmbedding(sessionID, i, vector, fmt.Sprintf("hash-%d", i), "openai/text-embedding-3-small"); err != nil {
			t.Fatalf("store embedding %d: %v", i, err)
		}
	}
	
	// Verify stored
	embeddings, err := store.GetEmbeddings(sessionID)
	if err != nil {
		t.Fatalf("get embeddings before delete: %v", err)
	}
	if len(embeddings) != 3 {
		t.Fatalf("expected 3 embeddings, got %d", len(embeddings))
	}
	
	// Delete
	if err := store.DeleteEmbeddings(sessionID); err != nil {
		t.Fatalf("delete embeddings: %v", err)
	}
	
	// Verify deleted
	embeddings, err = store.GetEmbeddings(sessionID)
	if err != nil {
		t.Fatalf("get embeddings after delete: %v", err)
	}
	if len(embeddings) != 0 {
		t.Fatalf("expected 0 embeddings after delete, got %d", len(embeddings))
	}
}

func TestGetAllEmbeddings(t *testing.T) {
	store := newTestSQLiteStore(t)
	
	// Create two sessions with embeddings
	for sessionIdx := 0; sessionIdx < 2; sessionIdx++ {
		sessionID := fmt.Sprintf("embed-all-%d", sessionIdx)
		startedAt := time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC).Add(time.Duration(sessionIdx) * time.Hour)
		
		if err := store.CreateSession(sessionID, startedAt); err != nil {
			t.Fatalf("create session %s: %v", sessionID, err)
		}
		
		// Store 2 embeddings per session
		for chunkIdx := 0; chunkIdx < 2; chunkIdx++ {
			vector := []float32{float32(sessionIdx)*0.1 + float32(chunkIdx)*0.01}
			if err := store.StoreEmbedding(sessionID, chunkIdx, vector, fmt.Sprintf("hash-%d-%d", sessionIdx, chunkIdx), "openai/text-embedding-3-small"); err != nil {
				t.Fatalf("store embedding %s[%d]: %v", sessionID, chunkIdx, err)
			}
		}
	}
	
	// Get all embeddings
	all, err := store.GetAllEmbeddings()
	if err != nil {
		t.Fatalf("get all embeddings: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 total embeddings, got %d", len(all))
	}
	
	// Verify we have embeddings from both sessions
	sessionIDs := make(map[string]bool)
	for _, emb := range all {
		sessionIDs[emb.SessionID] = true
	}
	if len(sessionIDs) != 2 {
		t.Fatalf("expected embeddings from 2 sessions, got %d unique sessions", len(sessionIDs))
	}
}
