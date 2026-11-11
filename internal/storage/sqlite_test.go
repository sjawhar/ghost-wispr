package storage

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
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

func setEnvoyPublishStatus(store *SQLiteStore, sessionID, status string) error {
	session, err := store.GetSession(sessionID)
	if err != nil {
		return err
	}
	updated, err := store.UpdateEnvoyPublishStatusAtVersion(sessionID, session.SummaryVersion, status)
	if err != nil {
		return err
	}
	if !updated {
		return fmt.Errorf("session %s changed before envoy publish status update", sessionID)
	}
	return nil
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

	if err := store.UpdateSummary(sessionID, "Meeting Notes", "## Summary\n- done", SummaryCompleted, "default", "{}"); err != nil {
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

	if err := store.UpdateSummary(sessionID, "Concise Notes", "## Summary\n- done", SummaryCompleted, "concise", "{}"); err != nil {
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

func TestSummaryVersionIncrements(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "summary-version"
	if err := store.CreateSession(sessionID, time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.EndSession(sessionID, time.Date(2026, 11, 10, 12, 31, 45, 0, time.UTC), ""); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	version := func(step string) int {
		t.Helper()
		sess, err := store.GetSession(sessionID)
		if err != nil {
			t.Fatalf("GetSession %s: %v", step, err)
		}
		return sess.SummaryVersion
	}
	if got := version("before summary"); got != 1 {
		t.Fatalf("initial summary version = %d, want 1", got)
	}

	// The first summary keeps version 1.
	if err := store.StartSummary(sessionID); err != nil {
		t.Fatalf("StartSummary first: %v", err)
	}
	if err := store.UpdateSummary(sessionID, "First", "First summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary first: %v", err)
	}
	if got := version("after first summary"); got != 1 {
		t.Fatalf("first summary version = %d, want 1", got)
	}

	// A regeneration through StartSummary bumps it once.
	if err := store.StartSummary(sessionID); err != nil {
		t.Fatalf("StartSummary regenerated: %v", err)
	}
	if err := store.UpdateSummary(sessionID, "Corrected", "Corrected summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary regenerated: %v", err)
	}
	if got := version("after regenerated summary"); got != 2 {
		t.Fatalf("regenerated summary version = %d, want 2", got)
	}
	if err := setEnvoyPublishStatus(store, sessionID, EnvoyPublishPublished); err != nil {
		t.Fatalf("mark regenerated summary published: %v", err)
	}

	// A completed summary rewritten without StartSummary (the batch path) bumps it too.
	if err := store.UpdateSummary(sessionID, "Batched", "Batched summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary batched: %v", err)
	}
	if got := version("after batched summary"); got != 3 {
		t.Fatalf("batched summary version = %d, want 3", got)
	}
	sess, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession after batched summary: %v", err)
	}
	if sess.EnvoyPublishStatus != EnvoyPublishPending {
		t.Fatalf("batched summary publish status = %q, want %q", sess.EnvoyPublishStatus, EnvoyPublishPending)
	}
	ids, err := store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatalf("GetSessionsNeedingEnvoyPublish after batched summary: %v", err)
	}
	if len(ids) != 1 || ids[0] != sessionID {
		t.Fatalf("sessions needing Envoy publish = %v, want [%s]", ids, sessionID)
	}
	if err := setEnvoyPublishStatus(store, sessionID, EnvoyPublishPublished); err != nil {
		t.Fatalf("mark batched summary published: %v", err)
	}

	// Re-reading the completed batch result is not a regeneration.
	if err := store.UpdateSummary(sessionID, "Batched", "Batched summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary repeated batch result: %v", err)
	}
	if got := version("after repeated batch result"); got != 3 {
		t.Fatalf("repeated batch summary version = %d, want 3", got)
	}
	sess, err = store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession after repeated batch result: %v", err)
	}
	if sess.EnvoyPublishStatus != EnvoyPublishPublished {
		t.Fatalf("repeated batch publish status = %q, want %q", sess.EnvoyPublishStatus, EnvoyPublishPublished)
	}

	for _, change := range []struct {
		name  string
		apply func(*Session, *string, *string, *string)
	}{
		{"summary", func(_ *Session, summary, _ *string, _ *string) { *summary = "Summary text changed" }},
		{"preset", func(_ *Session, _ *string, preset, _ *string) { *preset = "detailed" }},
		{"speaker names", func(_ *Session, _ *string, _ *string, speakers *string) { *speakers = `{"0":{"name":"Sam"}}` }},
	} {
		t.Run("same title "+change.name, func(t *testing.T) {
			current, err := store.GetSession(sessionID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if err := setEnvoyPublishStatus(store, sessionID, EnvoyPublishPublished); err != nil {
				t.Fatalf("mark summary published: %v", err)
			}
			summary, preset, speakers := current.Summary, current.SummaryPreset, current.SpeakerNames
			change.apply(&current, &summary, &preset, &speakers)
			if err := store.UpdateSummary(sessionID, current.Title, summary, SummaryCompleted, preset, speakers); err != nil {
				t.Fatalf("UpdateSummary: %v", err)
			}
			updated, err := store.GetSession(sessionID)
			if err != nil {
				t.Fatalf("GetSession updated: %v", err)
			}
			if updated.SummaryVersion != current.SummaryVersion+1 {
				t.Fatalf("summary version = %d, want %d", updated.SummaryVersion, current.SummaryVersion+1)
			}
			if updated.EnvoyPublishStatus != EnvoyPublishPending {
				t.Fatalf("envoy publish status = %q, want %q", updated.EnvoyPublishStatus, EnvoyPublishPending)
			}
		})
	}
}

func TestSummaryVersionMigratesExistingSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary-version.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	startedAt := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	if err := store.CreateSession("existing", startedAt); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.EndSession("existing", startedAt.Add(time.Minute), ""); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := store.UpdateSummary("existing", "Existing", "Existing summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close current store: %v", err)
	}

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacy.Exec(`ALTER TABLE sessions DROP COLUMN summary_version`); err != nil {
		_ = legacy.Close()
		t.Fatalf("remove summary_version from legacy database: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	migrated, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	sess, err := migrated.GetSession("existing")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.SummaryVersion != 1 {
		t.Fatalf("migrated summary version = %d, want 1", sess.SummaryVersion)
	}
	sessions, err := migrated.GetSessionsByDate("2026-11-10", false)
	if err != nil {
		t.Fatalf("GetSessionsByDate: %v", err)
	}
	if len(sessions) != 1 || sessions[0].SummaryVersion != 1 {
		t.Fatalf("migrated date query = %#v, want one session at version 1", sessions)
	}
}

func TestOpeningStoreMovesRejectedEnvoyPublishToPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	if err := store.CreateSession("session-1", time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET envoy_publish_status = 'rejected' WHERE id = ?`, "session-1"); err != nil {
		t.Fatalf("mark rejected: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	session, err := reopened.GetSession("session-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.EnvoyPublishStatus != EnvoyPublishPending {
		t.Fatalf("envoy publish status = %q, want %q", session.EnvoyPublishStatus, EnvoyPublishPending)
	}
}

func TestSpeakerNames(t *testing.T) {
	store := newTestSQLiteStore(t)

	startedAt := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	sessionID := startedAt.Format("20060102150405")
	if err := store.CreateSession(sessionID, startedAt); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	t.Run("UpdateSummary persists speaker_names", func(t *testing.T) {
		speakerNames := `{"1":{"name":"Ben","confidence":"mentioned"}}`
		if err := store.UpdateSummary(sessionID, "Speaker notes", "## Summary\n- discussed", SummaryCompleted, "default", speakerNames); err != nil {
			t.Fatalf("UpdateSummary failed: %v", err)
		}

		session, err := store.GetSession(sessionID)
		if err != nil {
			t.Fatalf("GetSession failed: %v", err)
		}
		if session.SpeakerNames != speakerNames {
			t.Fatalf("expected speaker_names %q, got %q", speakerNames, session.SpeakerNames)
		}
	})

	t.Run("speaker_names defaults to empty JSON", func(t *testing.T) {
		emptyID := "speaker-default-empty"
		if err := store.CreateSession(emptyID, startedAt.Add(1*time.Minute)); err != nil {
			t.Fatalf("CreateSession failed: %v", err)
		}

		var speakerNames string
		if err := store.DB().QueryRow(`SELECT speaker_names FROM sessions WHERE id = ?`, emptyID).Scan(&speakerNames); err != nil {
			t.Fatalf("query speaker_names failed: %v", err)
		}
		if speakerNames != "{}" {
			t.Fatalf("expected default speaker_names {}, got %q", speakerNames)
		}
	})
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

// A session that ends without a saved recording can never be refined, so it
// ends with the terminal refinement status no_audio instead of staying pending.
// One that ends with its recording stays pending for batch refinement.
func TestEndSessionWithoutRecordingMarksRefinementNoAudio(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	for id, audioPath := range map[string]string{"20261106140930": "", "20261106140931": "/audio/20261106140931.wav"} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), audioPath); err != nil {
			t.Fatalf("EndSession %s: %v", id, err)
		}
		want := RefinementPending
		if audioPath == "" {
			want = RefinementNoAudio
		}
		session, err := store.GetSession(id)
		if err != nil {
			t.Fatalf("GetSession %s: %v", id, err)
		}
		if session.RefinementStatus != want {
			t.Errorf("session %s ended with recording %q: refinement_status %q, want %q", id, audioPath, session.RefinementStatus, want)
		}
	}
}

// A session left active by a restart is ended with no recording, so it is
// marked no_audio too.
func TestRecoverStaleSessionsMarksRefinementNoAudio(t *testing.T) {
	store := newTestSQLiteStore(t)
	if err := store.CreateSession("20261106140930", time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.RecoverStaleSessions(); err != nil {
		t.Fatalf("RecoverStaleSessions: %v", err)
	}
	session, err := store.GetSession("20261106140930")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.Status != SessionEnded || session.RefinementStatus != RefinementNoAudio {
		t.Errorf("recovered session is %q with refinement_status %q, want ended and %q", session.Status, session.RefinementStatus, RefinementNoAudio)
	}
}

// Sessions that ended without a saved recording before no_audio existed (the
// recordings given up since #35, or sessions recovered at startup) are moved
// off pending when the store opens; sessions with a recording, still active,
// or past pending are left alone.
func TestOpeningTheStoreMarksEarlierSessionsWithoutRecordingNoAudio(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	for id, audioPath := range map[string]string{"20261106140000": "", "20261106140100": "/audio/20261106140100.wav", "20261106140200": ""} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), audioPath); err != nil {
			t.Fatalf("EndSession %s: %v", id, err)
		}
	}
	if err := store.CreateSession("20261106140300", started); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// As before no_audio existed; one session without a recording was refined anyway.
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = 'pending'`); err != nil {
		t.Fatalf("reset refinement status: %v", err)
	}
	if err := store.UpdateRefinement("20261106140200", "refined", RefinementCompleted); err != nil {
		t.Fatalf("UpdateRefinement: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store = newStoreAt(t, path)
	for id, want := range map[string]string{
		"20261106140000": RefinementNoAudio,   // ended without a recording
		"20261106140100": RefinementPending,   // ended with one
		"20261106140200": RefinementCompleted, // past pending
		"20261106140300": RefinementPending,   // still active
	} {
		session, err := store.GetSession(id)
		if err != nil {
			t.Fatalf("GetSession %s: %v", id, err)
		}
		if session.RefinementStatus != want {
			t.Errorf("session %s refinement_status %q after reopening, want %q", id, session.RefinementStatus, want)
		}
	}
}

func newStoreAt(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// Recordings lists, oldest first, every session that names audio, and
// whether disk pressure may remove its recording: ended, not being refined,
// and synced when Drive sync gates GC. Any other sync state keeps the
// recording.
func TestRecordings(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	end := func(id string, at time.Duration, audioPath, syncState string) {
		t.Helper()
		if err := store.CreateSession(id, started.Add(at)); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
		if err := store.EndSession(id, started.Add(at+time.Minute), audioPath); err != nil {
			t.Fatalf("EndSession %s: %v", id, err)
		}
		if syncState != "" {
			syncTo(t, store, id, syncState, "folder", 0, "")
		}
	}
	end("20261106140300", 3*time.Minute, "/audio/c.mp3", SyncStateSynced)
	end("20261106140000", 0, "/audio/a.mp3,/audio/b.mp3", SyncStateSynced) // merged recordings
	end("20261106140100", time.Minute, "/audio/pending.mp3", "")
	end("20261106140200", 2*time.Minute, "/audio/failed.mp3", SyncStateFailed)
	end("20261106140400", 4*time.Minute, "", SyncStateSynced) // no recording
	end("20261106140500", 5*time.Minute, "/audio/discarded.mp3", SyncStateSynced)
	if err := store.DiscardSession("20261106140500"); err != nil {
		t.Fatalf("DiscardSession: %v", err)
	}
	end("20261106140600", 6*time.Minute, "/audio/refining.mp3", SyncStateSynced)
	if err := store.UpdateRefinement("20261106140600", "", RefinementRunning); err != nil {
		t.Fatalf("UpdateRefinement: %v", err)
	}
	if err := store.CreateSession("20261106140700", started.Add(7*time.Minute)); err != nil { // still recording
		t.Fatalf("CreateSession: %v", err)
	}

	for _, tc := range []struct {
		syncGated bool
		want      []SessionRecording
	}{
		{syncGated: true, want: []SessionRecording{
			{ID: "20261106140000", AudioPath: "/audio/a.mp3,/audio/b.mp3", Removable: true},
			{ID: "20261106140100", AudioPath: "/audio/pending.mp3"},
			{ID: "20261106140200", AudioPath: "/audio/failed.mp3"},
			{ID: "20261106140300", AudioPath: "/audio/c.mp3", Removable: true},
			{ID: "20261106140500", AudioPath: "/audio/discarded.mp3"},
			{ID: "20261106140600", AudioPath: "/audio/refining.mp3"},
		}},
		{syncGated: false, want: []SessionRecording{
			{ID: "20261106140000", AudioPath: "/audio/a.mp3,/audio/b.mp3", Removable: true},
			{ID: "20261106140100", AudioPath: "/audio/pending.mp3", Removable: true},
			{ID: "20261106140200", AudioPath: "/audio/failed.mp3", Removable: true},
			{ID: "20261106140300", AudioPath: "/audio/c.mp3", Removable: true},
			{ID: "20261106140500", AudioPath: "/audio/discarded.mp3"},
			{ID: "20261106140600", AudioPath: "/audio/refining.mp3"},
		}},
	} {
		got, err := store.Recordings(tc.syncGated)
		if err != nil {
			t.Fatalf("Recordings(%v): %v", tc.syncGated, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("Recordings(%v) = %v, want %v", tc.syncGated, got, tc.want)
		}
	}
}

// endedWith ends a new session id with audioPath and refinement status
// refinement.
func endedWith(t *testing.T, store *SQLiteStore, id, audioPath, refinement string) {
	t.Helper()
	started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("CreateSession %s: %v", id, err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), audioPath); err != nil {
		t.Fatalf("EndSession %s: %v", id, err)
	}
	if err := store.UpdateRefinement(id, "", refinement); err != nil {
		t.Fatalf("UpdateRefinement %s: %v", id, err)
	}
}

// expectAudio fails the test unless session id has this audio path, marker
// and refinement status.
func expectAudio(t *testing.T, store *SQLiteStore, id, audioPath, removedAt, refinement string) {
	t.Helper()
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession %s: %v", id, err)
	}
	if session.AudioPath != audioPath || session.AudioRemovedAt != removedAt || session.RefinementStatus != refinement {
		t.Errorf("session %s: audio_path %q, audio_removed_at %q, refinement_status %q; want %q, %q, %q", id,
			session.AudioPath, session.AudioRemovedAt, session.RefinementStatus, audioPath, removedAt, refinement)
	}
}

// MarkAudioRemoved records, before GC removes any file, that disk pressure is
// taking part or all of a session's audio and keeping the session: the audio
// path becomes what will be left, the marker is set, and a refinement that
// can no longer run (pending or failed, no recording left on disk) becomes
// no_audio. What is left may still name a file that is not on disk (on a disk
// not mounted for the moment): it stays named, and does not count as audio
// left. A completed refinement stays completed. It returns the row as it was,
// for RestoreAudio.
func TestMarkAudioRemoved(t *testing.T) {
	store := newTestSQLiteStore(t)
	removedAt := time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)
	marker := removedAt.Format(time.RFC3339Nano)
	for _, tc := range []struct {
		id, audioPath, refinement string
		left                      string // the audio path left after the removal
		audioLeft                 bool   // whether left names a file on disk
		wantRefinement            string
	}{
		{id: "20261106140000", audioPath: "/audio/a.mp3", refinement: RefinementPending, wantRefinement: RefinementNoAudio},
		{id: "20261106140100", audioPath: "/audio/b.mp3", refinement: RefinementFailed, wantRefinement: RefinementNoAudio},
		{id: "20261106140200", audioPath: "/audio/c.mp3", refinement: RefinementCompleted, wantRefinement: RefinementCompleted},
		{id: "20261106140300", audioPath: "/audio/d1.mp3,/audio/d2.mp3", refinement: RefinementFailed, left: "/audio/d2.mp3", audioLeft: true, wantRefinement: RefinementFailed},
		{id: "20261106140400", audioPath: "/audio/e1.mp3,/mnt/e2.mp3", refinement: RefinementPending, left: "/mnt/e2.mp3", wantRefinement: RefinementNoAudio},
	} {
		endedWith(t, store, tc.id, tc.audioPath, tc.refinement)
		removal, ok, err := store.MarkAudioRemoved(tc.id, tc.audioPath, tc.left, tc.audioLeft, false, removedAt)
		if err != nil || !ok {
			t.Fatalf("MarkAudioRemoved %s = %v, %v", tc.id, ok, err)
		}
		if want := (AudioRemoval{ID: tc.id, Before: tc.audioPath, Left: tc.left, RefinementBefore: tc.refinement, SyncStateBefore: SyncStatePending}); removal != want {
			t.Errorf("MarkAudioRemoved %s returned %+v, want %+v", tc.id, removal, want)
		}
		expectAudio(t, store, tc.id, tc.left, marker, tc.wantRefinement)
	}

	listed, err := store.GetSessionsByDate("2026-11-06", false)
	if err != nil {
		t.Fatalf("GetSessionsByDate: %v", err)
	}
	for _, session := range listed {
		if session.AudioRemovedAt == "" {
			t.Errorf("session list shows %s without the audio-removed marker", session.ID)
		}
	}
}

// MarkAudioRemoved changes nothing, and says so, unless the session may still
// have this audio removed: its audio path is still the one GC listed, it is
// ended, no refinement is reading it, and it is synced while sync gates GC.
func TestMarkAudioRemovedRefusesASessionThatChanged(t *testing.T) {
	store := newTestSQLiteStore(t)
	removedAt := time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)

	endedWith(t, store, "20261106140000", "/audio/a.mp3", RefinementPending)
	endedWith(t, store, "20261106140100", "/audio/b.mp3", RefinementRunning)
	endedWith(t, store, "20261106140200", "/audio/c.mp3", RefinementPending) // not synced
	if err := store.CreateSession("20261106140300", time.Date(2026, 11, 6, 14, 3, 0, 0, time.UTC)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, tc := range []struct {
		name, id, before string
		syncGated        bool
	}{
		{name: "audio path changed", id: "20261106140000", before: "/audio/other.mp3"},
		{name: "refinement reading it", id: "20261106140100", before: "/audio/b.mp3"},
		{name: "not synced while sync gates GC", id: "20261106140200", before: "/audio/c.mp3", syncGated: true},
		{name: "still recording", id: "20261106140300", before: ""},
		{name: "gone", id: "20261106990000", before: "/audio/z.mp3"},
	} {
		removal, ok, err := store.MarkAudioRemoved(tc.id, tc.before, "", false, tc.syncGated, removedAt)
		if err != nil || ok || removal != (AudioRemoval{}) {
			t.Errorf("%s: MarkAudioRemoved = %+v, %v, %v; want nothing done", tc.name, removal, ok, err)
		}
	}
	expectAudio(t, store, "20261106140000", "/audio/a.mp3", "", RefinementPending)
	expectAudio(t, store, "20261106140100", "/audio/b.mp3", "", RefinementRunning)
	expectAudio(t, store, "20261106140200", "/audio/c.mp3", "", RefinementPending)
}

// RestoreAudio puts back what MarkAudioRemoved took out for files GC could
// not remove after all. With everything put back the session is as it was,
// marker and refinement included; with part of it, the session names what
// is left, keeps the marker, and a refinement moved to no_audio goes back.
func TestRestoreAudio(t *testing.T) {
	store := newTestSQLiteStore(t)
	earlier := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	removedAt := time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)
	marker := removedAt.Format(time.RFC3339Nano)
	mark := func(id, before, left string, at time.Time) *AudioRemoval {
		t.Helper()
		removal, ok, err := store.MarkAudioRemoved(id, before, left, left != "", false, at)
		if err != nil || !ok {
			t.Fatalf("MarkAudioRemoved %s = %v, %v", id, ok, err)
		}
		return &removal
	}

	endedWith(t, store, "20261106140000", "/audio/a.mp3", RefinementPending)
	if err := store.RestoreAudio(mark("20261106140000", "/audio/a.mp3", "", removedAt), "/audio/a.mp3"); err != nil {
		t.Fatalf("RestoreAudio all: %v", err)
	}
	expectAudio(t, store, "20261106140000", "/audio/a.mp3", "", RefinementPending)

	endedWith(t, store, "20261106140100", "/audio/d1.mp3,/audio/d2.mp3", RefinementFailed)
	if err := store.RestoreAudio(mark("20261106140100", "/audio/d1.mp3,/audio/d2.mp3", "", removedAt), "/audio/d2.mp3"); err != nil {
		t.Fatalf("RestoreAudio part: %v", err)
	}
	expectAudio(t, store, "20261106140100", "/audio/d2.mp3", marker, RefinementFailed)

	// Its audio was partly removed an earlier pass: putting this pass's back
	// keeps the earlier marker.
	endedWith(t, store, "20261106140200", "/audio/e1.mp3,/audio/e2.mp3", RefinementCompleted)
	mark("20261106140200", "/audio/e1.mp3,/audio/e2.mp3", "/audio/e2.mp3", earlier)
	if err := store.RestoreAudio(mark("20261106140200", "/audio/e2.mp3", "", removedAt), "/audio/e2.mp3"); err != nil {
		t.Fatalf("RestoreAudio after an earlier pass: %v", err)
	}
	expectAudio(t, store, "20261106140200", "/audio/e2.mp3", earlier.Format(time.RFC3339Nano), RefinementCompleted)

	// The row changed since the mark: nothing is overwritten.
	endedWith(t, store, "20261106140300", "/audio/f.mp3", RefinementPending)
	removal := mark("20261106140300", "/audio/f.mp3", "", removedAt)
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ? WHERE id = ?`, "/audio/g.mp3", "20261106140300"); err != nil {
		t.Fatalf("change the row: %v", err)
	}
	if err := store.RestoreAudio(removal, "/audio/f.mp3"); err == nil {
		t.Error("RestoreAudio overwrote a row that changed since the mark")
	}
	expectAudio(t, store, "20261106140300", "/audio/g.mp3", marker, RefinementNoAudio)
}

// ClaimRefinement marks a refinement running only while the session still
// names the recording it will read; once GC has taken that recording, the
// refinement does not start. And GC takes nothing from a session whose
// refinement is running.
func TestClaimRefinement(t *testing.T) {
	store := newTestSQLiteStore(t)
	removedAt := time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)

	endedWith(t, store, "20261106140000", "/audio/a.mp3", RefinementFailed)
	if _, err := store.DB().Exec(`UPDATE sessions SET refined_transcript = 'stale' WHERE id = ?`, "20261106140000"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimRefinement("20261106140000", "/audio/a.mp3"); err != nil || !claimed {
		t.Fatalf("ClaimRefinement = %v, %v; want claimed", claimed, err)
	}
	if transcript, status, err := store.GetRefinement("20261106140000"); err != nil || transcript != "" || status != RefinementRunning {
		t.Errorf("claimed refinement: %q, %q, %v; want no transcript and running", transcript, status, err)
	}
	if _, ok, err := store.MarkAudioRemoved("20261106140000", "/audio/a.mp3", "", false, false, removedAt); err != nil || ok {
		t.Errorf("MarkAudioRemoved under a running refinement = %v, %v; want refused", ok, err)
	}

	endedWith(t, store, "20261106140100", "/audio/b.mp3", RefinementPending)
	if _, ok, err := store.MarkAudioRemoved("20261106140100", "/audio/b.mp3", "", false, false, removedAt); err != nil || !ok {
		t.Fatalf("MarkAudioRemoved = %v, %v", ok, err)
	}
	for _, audioPath := range []string{"/audio/b.mp3", ""} {
		if claimed, err := store.ClaimRefinement("20261106140100", audioPath); err != nil || claimed {
			t.Errorf("ClaimRefinement(%q) after GC removed the audio = %v, %v; want not claimed", audioPath, claimed, err)
		}
	}
	expectAudio(t, store, "20261106140100", "", removedAt.Format(time.RFC3339Nano), RefinementNoAudio)
	if claimed, err := store.ClaimRefinement("20261106990000", "/audio/z.mp3"); err != nil || claimed {
		t.Errorf("ClaimRefinement of a missing session = %v, %v; want not claimed", claimed, err)
	}
}

// DeleteEndedSession deletes a session for the age policy only while it is as
// GC read it: ended, and naming that audio path. A session merged since gave
// its recording to the merged session, which keeps both.
func TestDeleteEndedSession(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, "20261106140000", "/audio/a.mp3", RefinementCompleted)
	if deleted, err := store.DeleteEndedSession("20261106140000", "/audio/other.mp3", false); err != nil || deleted {
		t.Errorf("DeleteEndedSession with another audio path = %v, %v; want kept", deleted, err)
	}
	if _, err := store.GetSession("20261106140000"); err != nil {
		t.Fatalf("session deleted: %v", err)
	}
	if deleted, err := store.DeleteEndedSession("20261106140000", "/audio/a.mp3", false); err != nil || !deleted {
		t.Errorf("DeleteEndedSession = %v, %v; want deleted", deleted, err)
	}
	if _, err := store.GetSession("20261106140000"); err == nil {
		t.Error("session still there")
	}

	endedWith(t, store, "20261106140100", "/audio/b.mp3", RefinementCompleted)
	if err := store.MergeSessions("20261106140100-merged", []string{"20261106140100"}, time.Date(2026, 11, 6, 14, 1, 0, 0, time.UTC), time.Date(2026, 11, 6, 14, 2, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MergeSessions: %v", err)
	}
	if deleted, err := store.DeleteEndedSession("20261106140100", "/audio/b.mp3", false); err != nil || deleted {
		t.Errorf("DeleteEndedSession of a merged source = %v, %v; want kept", deleted, err)
	}
	if deleted, err := store.DeleteEndedSession("20261106990000", "", false); err != nil || deleted {
		t.Errorf("DeleteEndedSession of a missing session = %v, %v; want nothing", deleted, err)
	}
}

// MarkSynced marks a session synced only while unchanged reports true for
// the session and its segments as they are now, read in the same
// transaction as the update: a sync whose session changed during the upload
// (its audio path, its summary, its transcript) leaves it as it was, for the
// next sync.
func TestMarkSynced(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, "20261106140000", "/audio/a.mp3,/mnt/b.mp3", RefinementPending)
	if err := store.UpdateSummary("20261106140000", "", "The current summary.", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary: %v", err)
	}
	if err := store.AppendSegment("20261106140000", transcribe.Segment{Text: "said", EndTime: 1, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("AppendSegment: %v", err)
	}
	syncTo(t, store, "20261106140000", SyncStateRetryScheduled, "", 2, "")
	if _, ok, err := store.ClaimSync("20261106140000", SyncStateRetryScheduled, "sync-1", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("ClaimSync = %v, %v", ok, err)
	}
	var seen Session
	var seenSegments []transcribe.Segment
	if ok, err := store.MarkSynced("20261106140000", "sync-1", "folder-1", func(now Session, segments []transcribe.Segment) bool {
		seen, seenSegments = now, segments
		return false
	}); err != nil || ok {
		t.Errorf("MarkSynced of a changed session = %v, %v; want refused", ok, err)
	}
	if seen.Summary != "The current summary." || seen.AudioPath != "/audio/a.mp3,/mnt/b.mp3" || len(seenSegments) != 1 || seenSegments[0].Text != "said" {
		t.Errorf("MarkSynced checked summary %q, audio path %q, segments %+v; want the session as it is", seen.Summary, seen.AudioPath, seenSegments)
	}
	if session, _ := store.GetSession("20261106140000"); session.SyncState != SyncStateSyncing {
		t.Errorf("refused MarkSynced left sync_state %q, want %q", session.SyncState, SyncStateSyncing)
	}
	if ok, err := store.MarkSynced("20261106140000", "sync-1", "folder-1", func(Session, []transcribe.Segment) bool { return true }); err != nil || !ok {
		t.Fatalf("MarkSynced = %v, %v; want marked", ok, err)
	}
	session, err := store.GetSession("20261106140000")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.SyncState != SyncStateSynced || session.SyncStatus != SyncSynced || session.GDriveFolderID != "folder-1" || session.RetryCount != 0 {
		t.Errorf("after MarkSynced: %q/%q in %q, retry %d; want synced in folder-1, retry 0", session.SyncState, session.SyncStatus, session.GDriveFolderID, session.RetryCount)
	}
	if ok, err := store.MarkSynced("20261106990000", "sync-1", "folder-1", func(Session, []transcribe.Segment) bool { return true }); err == nil || ok {
		t.Errorf("MarkSynced of a missing session = %v, %v; want an error", ok, err)
	}
}

// A sync that finished while GC had a session's row marked uploaded the row
// as marked; when GC puts back a recording it could not remove, the session
// names a recording Drive may not have, so RestoreAudio reopens the sync
// (pending, its folder kept). A session synced before the mark, as GC gated
// on sync finds them, stays synced: nothing synced it meanwhile.
func TestRestoreAudioReopensASyncThatRanMeanwhile(t *testing.T) {
	store := newTestSQLiteStore(t)
	removedAt := time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)
	mark := func(id string) *AudioRemoval {
		t.Helper()
		removal, ok, err := store.MarkAudioRemoved(id, "/audio/"+id+".mp3,/mnt/"+id+".mp3", "", false, false, removedAt)
		if err != nil || !ok {
			t.Fatalf("MarkAudioRemoved %s = %v, %v", id, ok, err)
		}
		return &removal
	}

	endedWith(t, store, "20261106140000", "/audio/20261106140000.mp3,/mnt/20261106140000.mp3", RefinementPending)
	removal := mark("20261106140000")
	syncTo(t, store, "20261106140000", SyncStateSynced, "folder-1", 0, "") // a sync ran meanwhile
	if err := store.RestoreAudio(removal, "/mnt/20261106140000.mp3"); err != nil {
		t.Fatalf("RestoreAudio: %v", err)
	}
	if session, _ := store.GetSession("20261106140000"); session.SyncState != SyncStatePending || session.SyncStatus != SyncPending || session.GDriveFolderID != "folder-1" {
		t.Errorf("after the put-back: %q/%q in %q; want pending in folder-1", session.SyncState, session.SyncStatus, session.GDriveFolderID)
	}

	endedWith(t, store, "20261106140100", "/audio/20261106140100.mp3,/mnt/20261106140100.mp3", RefinementPending)
	syncTo(t, store, "20261106140100", SyncStateSynced, "folder-2", 0, "") // synced before the mark
	if err := store.RestoreAudio(mark("20261106140100"), "/mnt/20261106140100.mp3"); err != nil {
		t.Fatalf("RestoreAudio: %v", err)
	}
	if session, _ := store.GetSession("20261106140100"); session.SyncState != SyncStateSynced {
		t.Errorf("a session synced before the mark: %q after the put-back, want still synced", session.SyncState)
	}
}

// RestoreAudio refuses a session merged since the mark: a merged source names
// no audio, its merged session has it.
func TestRestoreAudioRefusesAMergedSource(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, "20261106140000", "/audio/a.mp3", RefinementPending)
	removal, ok, err := store.MarkAudioRemoved("20261106140000", "/audio/a.mp3", "", false, false, time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC))
	if err != nil || !ok {
		t.Fatalf("MarkAudioRemoved = %v, %v", ok, err)
	}
	if err := store.MergeSessions("20261106140000-merged", []string{"20261106140000"}, time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC), time.Date(2026, 11, 6, 14, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MergeSessions: %v", err)
	}
	if err := store.RestoreAudio(&removal, "/audio/a.mp3"); err == nil {
		t.Error("RestoreAudio named audio on a merged source")
	}
	if source, err := store.GetSession("20261106140000"); err != nil || source.AudioPath != "" {
		t.Errorf("merged source audio_path %q (%v), want none", source.AudioPath, err)
	}
}

// A refinement left running by a stop of the app is not running: when the
// app starts, it is marked failed (which offers a retry), so GC, which leaves
// a running refinement's audio and session alone, is not held off for good.
func TestRecoverStaleRefinements(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, "20261106140000", "/audio/a.mp3", RefinementRunning)
	endedWith(t, store, "20261106140100", "/audio/b.mp3", RefinementCompleted)
	if err := store.RecoverStaleRefinements(); err != nil {
		t.Fatalf("RecoverStaleRefinements: %v", err)
	}
	expectAudio(t, store, "20261106140000", "/audio/a.mp3", "", RefinementFailed)
	expectAudio(t, store, "20261106140100", "/audio/b.mp3", "", RefinementCompleted)
}

// Drive sync used to upload no audio for a merged session (it stat'ed the
// joined path of its recordings), yet marked it synced. The first time the
// store opens with per-recording uploads, synced merged sessions go back to
// pending sync and keep their Drive folder, so the sync adds the missing audio
// to it: those naming several recordings, and those disk pressure had already
// cut down to one, which never reached Drive either. A session with one
// recording of its own is untouched, and later opens change nothing.
func TestOpeningTheStoreResyncsMergedSessionsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := newStoreAt(t, path)
	endedWith(t, store, "20261106140000-merged", "/audio/a.mp3,/audio/b.mp3", RefinementCompleted)
	endedWith(t, store, "20261106140100", "/audio/c.mp3", RefinementCompleted)
	started := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	for i, id := range []string{"20261106150000", "20261106151000"} {
		at := started.Add(time.Duration(i) * 10 * time.Minute)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatalf("EndSession %s: %v", id, err)
		}
	}
	if err := store.MergeSessions("20261106150000-merged", []string{"20261106150000", "20261106151000"}, started, started.Add(11*time.Minute)); err != nil {
		t.Fatalf("MergeSessions: %v", err)
	}
	// Disk pressure on 5df1e1e removed one of its two recordings.
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ?, audio_removed_at = ? WHERE id = ?`,
		"/audio/20261106151000.mp3", "2026-11-07T09:30:00Z", "20261106150000-merged"); err != nil {
		t.Fatalf("remove part of the merged audio: %v", err)
	}
	for id, folder := range map[string]string{"20261106140000-merged": "folder-1", "20261106140100": "folder-2", "20261106150000-merged": "folder-3"} {
		syncTo(t, store, id, SyncStateSynced, folder, 0, "")
	}
	// As a database from before per-recording uploads: the migration has not run.
	if _, err := store.DB().Exec(`DELETE FROM migrations WHERE name = ?`, "resync-merged-session-audio"); err != nil {
		t.Fatalf("forget the migration: %v", err)
	}
	reopen := func() {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		store = newStoreAt(t, path)
	}
	expectSync := func(id, status, state, folder string) {
		t.Helper()
		session, err := store.GetSession(id)
		if err != nil {
			t.Fatalf("GetSession %s: %v", id, err)
		}
		if session.SyncStatus != status || session.SyncState != state || session.GDriveFolderID != folder {
			t.Errorf("session %s: sync %q/%q in %q, want %q/%q in %q", id, session.SyncStatus, session.SyncState, session.GDriveFolderID, status, state, folder)
		}
	}

	reopen()
	expectSync("20261106140000-merged", SyncPending, SyncStatePending, "folder-1")
	expectSync("20261106150000-merged", SyncPending, SyncStatePending, "folder-3")
	expectSync("20261106140100", SyncSynced, SyncStateSynced, "folder-2")

	for id, folder := range map[string]string{"20261106140000-merged": "folder-1", "20261106150000-merged": "folder-3"} {
		syncTo(t, store, id, SyncStateSynced, folder, 0, "")
	}
	reopen()
	expectSync("20261106140000-merged", SyncSynced, SyncStateSynced, "folder-1")
	expectSync("20261106150000-merged", SyncSynced, SyncStateSynced, "folder-3")
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
	if err := store.UpdateSummary(sessionID, "Test Title", "Test summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("update summary: %v", err)
	}

	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatalf("get sessions needing sync: %v", err)
	}
	if len(ids) != 1 || ids[0] != sessionID {
		t.Fatalf("expected [%s], got %v", sessionID, ids)
	}

	syncTo(t, store, sessionID, SyncStateSynced, "drive-folder-id-123", 0, "")

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

func TestEnvoyPublishStatusTracking(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "envoy-test-1"
	started := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(30*time.Second), "data/audio/envoy-test-1.mp3"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(sessionID, "Test Title", "Test summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("update summary: %v", err)
	}

	ids, err := store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatalf("get sessions needing envoy publish: %v", err)
	}
	if len(ids) != 1 || ids[0] != sessionID {
		t.Fatalf("expected [%s], got %v", sessionID, ids)
	}

	if err := setEnvoyPublishStatus(store, sessionID, EnvoyPublishPublished); err != nil {
		t.Fatalf("update envoy publish status: %v", err)
	}

	ids, err = store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatalf("get sessions needing envoy publish after update: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected empty, got %v", ids)
	}

	sess, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.EnvoyPublishStatus != EnvoyPublishPublished {
		t.Fatalf("expected envoy_publish_status %q, got %q", EnvoyPublishPublished, sess.EnvoyPublishStatus)
	}

	if err := setEnvoyPublishStatus(store, sessionID, EnvoyPublishFailed); err != nil {
		t.Fatalf("mark envoy publish failed: %v", err)
	}
	ids, err = store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatalf("get sessions needing envoy publish after failure: %v", err)
	}
	if len(ids) != 1 || ids[0] != sessionID {
		t.Fatalf("failed summary should be swept again, got %v", ids)
	}
}

func TestEnvoyPublishStatusIsVersionAware(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "envoy-version"
	started := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(time.Minute), ""); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := store.UpdateSummary(sessionID, "Title", "Summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary: %v", err)
	}

	updated, err := store.UpdateEnvoyPublishStatusAtVersion(sessionID, 2, EnvoyPublishPublished)
	if err != nil {
		t.Fatalf("UpdateEnvoyPublishStatusAtVersion stale: %v", err)
	}
	if updated {
		t.Fatal("stale summary version updated the delivery status")
	}
	updated, err = store.UpdateEnvoyPublishStatusAtVersion(sessionID, 1, EnvoyPublishFailed)
	if err != nil {
		t.Fatalf("UpdateEnvoyPublishStatusAtVersion current: %v", err)
	}
	if !updated {
		t.Fatal("current summary version did not update the delivery status")
	}
}

func TestStartSummaryResetsEnvoyPublishStatus(t *testing.T) {
	store := newTestSQLiteStore(t)
	sessionID := "envoy-reset-1"
	started := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(30*time.Second), "data/audio/envoy-reset-1.mp3"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(sessionID, "Published Title", "Published summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("update summary completed: %v", err)
	}
	if err := setEnvoyPublishStatus(store, sessionID, EnvoyPublishPublished); err != nil {
		t.Fatalf("update envoy publish status: %v", err)
	}

	if err := store.StartSummary(sessionID); err != nil {
		t.Fatalf("start summary: %v", err)
	}

	sess, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.EnvoyPublishStatus != EnvoyPublishPending {
		t.Fatalf("expected envoy_publish_status reset to %q, got %q", EnvoyPublishPending, sess.EnvoyPublishStatus)
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
	// Summarized, then synced, as the app does it: a summary changed after
	// the sync sends the session back to sync.
	if err := store.UpdateSummary("gc-eligible", "", "test summary", "completed", "default", "{}"); err != nil {
		t.Fatal(err)
	}
	syncTo(t, store, "gc-eligible", SyncStateSynced, "folder-1", 0, "")

	if err := store.CreateSession("gc-unsynced", old); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gc-unsynced", old.Add(time.Minute), "data/audio/gc-unsynced.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummary("gc-unsynced", "", "test summary", "completed", "default", "{}"); err != nil {
		t.Fatal(err)
	}

	if err := store.CreateSession("gc-recent", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gc-recent", now.Add(-30*time.Minute), "data/audio/gc-recent.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummary("gc-recent", "", "test summary", "completed", "default", "{}"); err != nil {
		t.Fatal(err)
	}
	syncTo(t, store, "gc-recent", SyncStateSynced, "folder-2", 0, "")

	ids, err := store.GetGCEligibleSessions(30, true)
	if err != nil {
		t.Fatalf("get gc eligible: %v", err)
	}
	if len(ids) != 1 || ids[0] != "gc-eligible" {
		t.Fatalf("expected [gc-eligible], got %v", ids)
	}

	ids, err = store.GetGCEligibleSessions(30, false)
	if err != nil {
		t.Fatalf("get gc eligible no gate: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 sessions, got %d: %v", len(ids), ids)
	}
}

// summarizedSession ends session id, started at startedAt, with no recording
// and a completed summary, synced to Drive: the age policy deletes it once it
// is old enough.
func summarizedSession(t *testing.T, store *SQLiteStore, id string, startedAt time.Time) {
	t.Helper()
	if err := store.CreateSession(id, startedAt); err != nil {
		t.Fatalf("CreateSession %s: %v", id, err)
	}
	if err := store.EndSession(id, startedAt.Add(time.Minute), ""); err != nil {
		t.Fatalf("EndSession %s: %v", id, err)
	}
	if err := store.UpdateSummary(id, "", "A summary.", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary %s: %v", id, err)
	}
	syncTo(t, store, id, SyncStateSynced, "folder", 0, "")
}

// No age limit can wrap the cutoff round into the future. Past the longest
// age a time.Duration holds (106,751 days), an hour-old session counted as
// older than the limit, so the age policy deleted every summarized session.
// A limit longer than any session has existed returns none.
func TestGetGCEligibleSessionsNeverWrapsTheAgeLimit(t *testing.T) {
	store := newTestSQLiteStore(t)
	now := time.Now().UTC()
	summarizedSession(t, store, "hour-old", now.Add(-time.Hour))
	summarizedSession(t, store, "45-days-old", now.Add(-45*24*time.Hour))

	if ids, err := store.GetGCEligibleSessions(30, true); err != nil || !slices.Equal(ids, []string{"45-days-old"}) {
		t.Fatalf("GetGCEligibleSessions(30 days) = %v, %v; want [45-days-old]", ids, err)
	}
	for _, days := range []int{106751, 106752, 999999, 1000000, math.MaxInt32, math.MaxInt} {
		ids, err := store.GetGCEligibleSessions(days, true)
		if err != nil {
			t.Fatalf("GetGCEligibleSessions(%d days): %v", days, err)
		}
		if len(ids) != 0 {
			t.Errorf("GetGCEligibleSessions(%d days) = %v, want none: no session is that old", days, ids)
		}
	}
}

// An age limit of 0 is no limit: no session is ever old enough. (The config
// turns a negative limit into the default before it gets here; the store
// treats one as no limit too.)
func TestGetGCEligibleSessionsWithNoAgeLimitReturnsNone(t *testing.T) {
	store := newTestSQLiteStore(t)
	summarizedSession(t, store, "20160101120000", time.Now().UTC().Add(-3650*24*time.Hour))

	for _, days := range []int{0, -1} {
		ids, err := store.GetGCEligibleSessions(days, true)
		if err != nil {
			t.Fatalf("GetGCEligibleSessions(%d days): %v", days, err)
		}
		if len(ids) != 0 {
			t.Errorf("GetGCEligibleSessions(%d days) = %v, want none: there is no age limit", days, ids)
		}
	}
}

// A session restored from Drive keeps the time it started, but its age for
// retention runs from the restore: the GC pass after a restore must not
// delete it again. It becomes old once the restore is.
func TestGetGCEligibleSessionsMeasuresARestoredSessionFromItsRestore(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Now().UTC().Add(-400 * 24 * time.Hour).Truncate(time.Second)
	ended := started.Add(time.Hour)
	restored := Session{
		ID: "20251001120000", StartedAt: started, EndedAt: &ended, Status: SessionEnded,
		Summary: "A summary.", SummaryStatus: SummaryCompleted, SyncStatus: SyncSynced,
	}
	if err := store.ImportSession(&restored, nil); err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	summarizedSession(t, store, "20251001130000", started.Add(time.Hour)) // recorded here, never restored

	ids, err := store.GetGCEligibleSessions(30, true)
	if err != nil {
		t.Fatalf("GetGCEligibleSessions: %v", err)
	}
	if !slices.Equal(ids, []string{"20251001130000"}) {
		t.Errorf("GetGCEligibleSessions(30 days) right after a restore = %v, want only the session that was never restored", ids)
	}
	session, err := store.GetSession(restored.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !session.StartedAt.Equal(started) {
		t.Errorf("restored session started at %s, want %s", session.StartedAt, started)
	}

	if _, err := store.DB().Exec(`UPDATE sessions SET restored_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-31*24*time.Hour).Format(time.RFC3339Nano), restored.ID); err != nil {
		t.Fatalf("age the restore: %v", err)
	}
	ids, err = store.GetGCEligibleSessions(30, true)
	if err != nil {
		t.Fatalf("GetGCEligibleSessions: %v", err)
	}
	if !slices.Equal(ids, []string{restored.ID, "20251001130000"}) {
		t.Errorf("GetGCEligibleSessions(30 days) 31 days after a restore = %v, want both sessions", ids)
	}
}

// A merge moves the source sessions' recordings to the merged session, so
// each file has one session naming it: removing the merged session's audio
// cannot leave a source naming a file that is gone.
func TestMergeSessionsMovesTheRecordingsToTheMergedSession(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"20261106100000", "20261106101000"} {
		at := started.Add(time.Duration(i) * 10 * time.Minute)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "data/audio/"+id+".mp3"); err != nil {
			t.Fatalf("EndSession %s: %v", id, err)
		}
	}
	if err := store.MergeSessions("20261106100000-merged", []string{"20261106100000", "20261106101000"}, started, started.Add(11*time.Minute)); err != nil {
		t.Fatalf("MergeSessions: %v", err)
	}

	merged, err := store.GetSession("20261106100000-merged")
	if err != nil {
		t.Fatalf("GetSession merged: %v", err)
	}
	if want := "data/audio/20261106100000.mp3,data/audio/20261106101000.mp3"; merged.AudioPath != want {
		t.Errorf("merged session audio_path %q, want %q", merged.AudioPath, want)
	}
	for _, id := range []string{"20261106100000", "20261106101000"} {
		source, err := store.GetSession(id)
		if err != nil {
			t.Fatalf("GetSession %s: %v", id, err)
		}
		if source.AudioPath != "" {
			t.Errorf("merged source %s still names %q; the merged session has it", id, source.AudioPath)
		}
	}
}

// Sources merged before merges moved their recordings still name them: when
// the store opens, they give them up to the merged session, which names them
// too.
func TestOpeningTheStoreMovesRecordingsOffMergedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := newStoreAt(t, path)
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("20261106100000", started); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.EndSession("20261106100000", started.Add(time.Minute), "data/audio/20261106100000.mp3"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := store.MergeSessions("20261106100000-merged", []string{"20261106100000"}, started, started.Add(time.Minute)); err != nil {
		t.Fatalf("MergeSessions: %v", err)
	}
	// As an earlier version left it.
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ? WHERE id = ?`, "data/audio/20261106100000.mp3", "20261106100000"); err != nil {
		t.Fatalf("restore the source's audio path: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store = newStoreAt(t, path)
	source, err := store.GetSession("20261106100000")
	if err != nil {
		t.Fatalf("GetSession source: %v", err)
	}
	if source.AudioPath != "" {
		t.Errorf("merged source still names %q after reopening", source.AudioPath)
	}
	merged, err := store.GetSession("20261106100000-merged")
	if err != nil {
		t.Fatalf("GetSession merged: %v", err)
	}
	if merged.AudioPath != "data/audio/20261106100000.mp3" {
		t.Errorf("merged session audio_path %q after reopening, want the source's recording", merged.AudioPath)
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
	syncToAt(t, store, sessionID, SyncStateRetryScheduled, "", 2, "temporary upload failure", attempt)

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
	syncToAt(t, store, sessionID, SyncStateRemoteDeleted, "", 0, "remote folder deleted", attempt)

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
	if err := store.UpdateSummary("fts-1", "Alpha planning", "Discussed alpha rollout", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("update summary fts-1: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET canonical_transcript = ? WHERE id = ?`, "alpha appears once in transcript", "fts-1"); err != nil {
		t.Fatalf("update canonical transcript fts-1: %v", err)
	}

	if err := store.CreateSession("fts-2", started.Add(time.Minute)); err != nil {
		t.Fatalf("create session fts-2: %v", err)
	}
	if err := store.UpdateSummary("fts-2", "Alpha alpha alpha", "Alpha mentioned repeatedly", SummaryCompleted, "default", "{}"); err != nil {
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
	if err := store.UpdateSummary("fts-sync", "Initial title", "Initial summary", SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("update summary initial: %v", err)
	}

	results, err := store.Search("delta", SearchOptions{})
	if err != nil {
		t.Fatalf("search initial: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no matches before update, got %d", len(results))
	}

	if err := store.UpdateSummary("fts-sync", "Delta title", "Summary with delta keyword", SummaryCompleted, "default", "{}"); err != nil {
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
	if err := store.UpdateSummary("fts-escape", "Alpha Operators", "Contains alpha keyword", SummaryCompleted, "default", "{}"); err != nil {
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
		if err := store.UpdateSummary(id, "Test", "test content", SummaryCompleted, "default", "{}"); err != nil {
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
		if err := store.UpdateSummary(id, "Test", "test content", SummaryCompleted, "default", "{}"); err != nil {
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
		if err := store.UpdateSummary(id, "Test", "test content", SummaryCompleted, preset, "{}"); err != nil {
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
		if err := store.UpdateSummary(id, "Test", "test content", SummaryCompleted, preset, "{}"); err != nil {
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
		if err := store.UpdateSummary(s.id, "Title "+s.id, "Summary", SummaryCompleted, s.preset, "{}"); err != nil {
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

func TestStoreEvent(t *testing.T) {
	store := newTestSQLiteStore(t)

	// Store events
	if err := store.StoreEvent("session_started", `{"session_id":"s1"}`); err != nil {
		t.Fatalf("StoreEvent session_started: %v", err)
	}
	if err := store.StoreEvent("session_ended", `{"session_id":"s1","duration":30}`); err != nil {
		t.Fatalf("StoreEvent session_ended: %v", err)
	}
	if err := store.StoreEvent("summary_ready", `{"session_id":"s1","title":"Test"}`); err != nil {
		t.Fatalf("StoreEvent summary_ready: %v", err)
	}

	// Retrieve all from cursor 0
	events, err := store.GetEventsSince(0, 100)
	if err != nil {
		t.Fatalf("GetEventsSince: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// Verify fields
	if events[0].EventType != "session_started" {
		t.Fatalf("expected event_type session_started, got %q", events[0].EventType)
	}
	if events[0].Payload != `{"session_id":"s1"}` {
		t.Fatalf("expected payload, got %q", events[0].Payload)
	}
	if events[0].ID <= 0 {
		t.Fatalf("expected positive ID, got %d", events[0].ID)
	}
	if events[0].CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}

	// IDs are ascending
	if events[1].ID <= events[0].ID {
		t.Fatalf("expected ascending IDs: %d <= %d", events[1].ID, events[0].ID)
	}
}

func TestPurgeOldEvents(t *testing.T) {
	store := newTestSQLiteStore(t)

	// Store an event
	if err := store.StoreEvent("session_started", `{"session_id":"s1"}`); err != nil {
		t.Fatalf("StoreEvent: %v", err)
	}

	// Backdate it to 10 days ago
	oldTime := time.Now().UTC().Add(-10 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := store.DB().Exec(`UPDATE mcp_events SET created_at = ?`, oldTime); err != nil {
		t.Fatalf("backdate event: %v", err)
	}

	// Store a recent event
	if err := store.StoreEvent("session_ended", `{"session_id":"s2"}`); err != nil {
		t.Fatalf("StoreEvent: %v", err)
	}

	// Purge events older than 7 days
	if err := store.PurgeOldEvents(7 * 24 * time.Hour); err != nil {
		t.Fatalf("PurgeOldEvents: %v", err)
	}

	// Only the recent event should remain
	events, err := store.GetEventsSince(0, 100)
	if err != nil {
		t.Fatalf("GetEventsSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event after purge, got %d", len(events))
	}
	if events[0].EventType != "session_ended" {
		t.Fatalf("expected session_ended to survive purge, got %q", events[0].EventType)
	}
}

func TestGetEventsSince_Pagination(t *testing.T) {
	store := newTestSQLiteStore(t)

	// Store 5 events
	for i := 0; i < 5; i++ {
		if err := store.StoreEvent("status_changed", fmt.Sprintf(`{"index":%d}`, i)); err != nil {
			t.Fatalf("StoreEvent %d: %v", i, err)
		}
	}

	// Get first 2
	events, err := store.GetEventsSince(0, 2)
	if err != nil {
		t.Fatalf("GetEventsSince page 1: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Get next 2 using cursor from last event
	cursor := events[1].ID
	events2, err := store.GetEventsSince(cursor, 2)
	if err != nil {
		t.Fatalf("GetEventsSince page 2: %v", err)
	}
	if len(events2) != 2 {
		t.Fatalf("expected 2 events on page 2, got %d", len(events2))
	}
	if events2[0].ID <= cursor {
		t.Fatalf("expected events after cursor %d, got ID %d", cursor, events2[0].ID)
	}

	// Get remaining
	cursor2 := events2[1].ID
	events3, err := store.GetEventsSince(cursor2, 2)
	if err != nil {
		t.Fatalf("GetEventsSince page 3: %v", err)
	}
	if len(events3) != 1 {
		t.Fatalf("expected 1 event on page 3, got %d", len(events3))
	}

	// Get past end — empty
	cursor3 := events3[0].ID
	events4, err := store.GetEventsSince(cursor3, 2)
	if err != nil {
		t.Fatalf("GetEventsSince past end: %v", err)
	}
	if len(events4) != 0 {
		t.Fatalf("expected 0 events past end, got %d", len(events4))
	}
}

func TestProviderModelsPersistWithSummaryAndRefinement(t *testing.T) {
	store := newTestSQLiteStore(t)
	const id = "provider-models"
	if err := store.CreateSession(id, time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.UpdateSummaryWithModel(id, "Model report", "Summary", SummaryCompleted, "default", "{}", "provider-summary-model"); err != nil {
		t.Fatalf("UpdateSummaryWithModel: %v", err)
	}
	if err := store.UpdateRefinementWithModel(id, "Refined transcript", RefinementCompleted, "provider-refinement-model"); err != nil {
		t.Fatalf("UpdateRefinementWithModel: %v", err)
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.SummaryModel != "provider-summary-model" {
		t.Fatalf("summary model = %q", sess.SummaryModel)
	}
	if sess.RefinementModel != "provider-refinement-model" {
		t.Fatalf("refinement model = %q", sess.RefinementModel)
	}
}
