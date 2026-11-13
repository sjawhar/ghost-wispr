package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const backupSecret = "backup-held-deletion-phrase-77c2e1"

// openWithSecret opens a store at path holding session "backed-up", whose
// middle segment, canonical transcript, summary and one live transcript event
// hold backupSecret, then closes it.
func openWithSecret(t *testing.T, path string) {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = segmentTestSession(t, store, "backed-up")
	segments, err := store.GetSegments("backed-up")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE segments SET text = ? WHERE id = ?`, backupSecret, segments[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Canonicalize("backed-up"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("backed-up", "Title", "The summary of "+backupSecret+".", SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("live_transcript", `{"type":"live_transcript","session_id":"backed-up","text":"`+backupSecret+`","start_time":2,"end_time":4}`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// deleteSecretFrom reopens the store at path (which takes the backup) and
// deletes the segment holding backupSecret.
func deleteSecretFrom(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	store := openStoreWithBackup(t, path)
	deleteBackedUpSecret(t, store)
	return store
}

// openStoreWithBackup reopens the store at path, which takes the backup, and
// checks that the backup holds backupSecret.
func openStoreWithBackup(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertBackupHolds(t, path, backupSecret, true)
	return store
}

// deleteBackedUpSecret deletes the segment of session "backed-up" holding
// backupSecret.
func deleteBackedUpSecret(t *testing.T, store *SQLiteStore) {
	t.Helper()
	segments, err := store.GetSegments("backed-up")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedAudioTestSegment(t, store, "backed-up", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatalf("delete refined segment: %v", err)
	}
}

// assertBackupHolds fails unless the backup beside the database at path, with
// any write-ahead log or index beside it, holds text (want) or does not
// (!want). The backup must exist and hold the kept text "after bridge", so
// the read is shown able to see text.
func assertBackupHolds(t *testing.T, path, text string, want bool) {
	t.Helper()
	backup := preMigrationBackupPath(path)
	contents, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	all := string(contents)
	for _, extra := range []string{backup + "-wal", backup + "-shm"} {
		if more, err := os.ReadFile(extra); err == nil {
			t.Logf("%s: %d bytes", filepath.Base(extra), len(more))
			all += string(more)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if !strings.Contains(all, "after bridge") {
		t.Fatal("control: the backup does not hold the kept text, so the read saw no data")
	}
	if got := strings.Contains(all, text); got != want {
		t.Errorf("backup holds %q = %v, want %v", text, got, want)
	}
}

// TestADeletionRefreshesABackupOfTheSameSchema fails if a deletion leaves its
// words in the pre-migration backup when no migration ran at this start (the
// backup then has the database's schema and is taken again).
func TestADeletionRefreshesABackupOfTheSameSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	deleteSecretFrom(t, path)
	assertBackupHolds(t, path, backupSecret, false)
}

// TestADeletionScrubsABackupOfAnOlderSchemaInPlace fails if a deletion
// leaves its words in the pre-migration backup when a migration ran at this
// start, or if it replaces that backup (the only copy in the old schema, which
// the previous build needs to start again) with the current database.
func TestADeletionScrubsABackupOfAnOlderSchemaInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	// The previous build's schema lacks a column this version adds.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`ALTER TABLE sessions DROP COLUMN drive_replace_error`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	deleteSecretFrom(t, path)
	assertBackupHolds(t, path, backupSecret, false)
	backup, err := sql.Open("sqlite", preMigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backup.Close() }()
	var columns int
	if err := backup.QueryRow(`SELECT count(*) FROM pragma_table_info('sessions') WHERE name = 'drive_replace_error'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Error("the backup of the older schema was replaced by the current database")
	}
	var segments int
	if err := backup.QueryRow(`SELECT count(*) FROM segments WHERE session_id = 'backed-up'`).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if segments != 2 {
		t.Errorf("the backup keeps %d of the session's segments, want the two not deleted", segments)
	}
}

// TestADeletionScrubsTheSpeakerNamesFromABackupOfAnOlderSchema fails if a
// deletion leaves the session's speaker names in a pre-migration backup it
// scrubs in place: the database clears them with the summary that named the
// speakers, and the names can come from the deleted words.
func TestADeletionScrubsTheSpeakerNamesFromABackupOfAnOlderSchema(t *testing.T) {
	const speaker = "speaker-named-in-deleted-words-41d9"
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`UPDATE sessions SET speaker_names = ? WHERE id = 'backed-up'`, `{"0":"`+speaker+`"}`); err != nil {
		t.Fatal(err)
	}
	// The previous build's schema lacks a column this version adds.
	if _, err := old.Exec(`ALTER TABLE sessions DROP COLUMN drive_replace_error`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := openStoreWithBackup(t, path)
	assertBackupHolds(t, path, speaker, true)
	deleteBackedUpSecret(t, store)
	assertBackupHolds(t, path, speaker, false)
}
