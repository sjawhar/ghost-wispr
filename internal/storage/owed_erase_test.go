package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// failVacuumInto makes the next n copies the backup takes fail as a full disk
// does, and restores the copy when the test ends.
func failVacuumInto(t *testing.T, n int) {
	t.Helper()
	original := vacuumInto
	t.Cleanup(func() { vacuumInto = original })
	vacuumInto = func(db *sql.DB, path string) error {
		if n > 0 {
			n--
			return fmt.Errorf("copy the database: %w", syscall.ENOSPC)
		}
		return original(db, path)
	}
}

// owedErasures returns the number of the last deletion whose erase from the
// write-ahead log and the backup is still owed (owedEraseKey), 0 when none
// is. In a new store the first deletion is number 1.
func owedErasures(t *testing.T, store *SQLiteStore) int {
	t.Helper()
	var owed int
	switch err := store.DB().QueryRow(`SELECT CAST(value AS INTEGER) FROM app_state WHERE key = ?`, owedEraseKey).Scan(&owed); {
	case errors.Is(err, sql.ErrNoRows):
		return 0
	case err != nil:
		t.Fatal(err)
	}
	return owed
}

// assertOwedRowsHoldNoText fails if a deleted-span row, or the owed-erase
// marker, holds any of the deleted words.
func assertOwedRowsHoldNoText(t *testing.T, store *SQLiteStore, secret string) {
	t.Helper()
	rows, err := store.DB().Query(`SELECT * FROM deleted_segments`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		if row := fmt.Sprint(values...); strings.Contains(row, secret) {
			t.Errorf("the deleted-span row holds the deleted words: %s", row)
		}
	}
	var marker string
	if err := store.DB().QueryRow(`SELECT value FROM app_state WHERE key = ?`, owedEraseKey).Scan(&marker); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if strings.Contains(marker, secret) {
		t.Errorf("the owed-erase marker holds the deleted words: %s", marker)
	}
}

// TestAFailedBackupEraseIsRetried fails if a deletion whose backup refresh
// fails (VACUUM INTO on a full disk) leaves the deleted words in the backup
// for good: the erase must be recorded as owed, without the text, and a retry
// must take the backup again once it can.
func TestAFailedBackupEraseIsRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	failVacuumInto(t, 1)

	segments, err := store.GetSegments("backed-up")
	if err != nil {
		t.Fatal(err)
	}
	err = store.DeleteSegment("backed-up", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"})
	if !errors.Is(err, ErrSegmentPostCommitMaintenance) {
		t.Fatalf("DeleteSegment with a full disk = %v, want %v", err, ErrSegmentPostCommitMaintenance)
	}
	assertBackupHolds(t, path, backupSecret, true)
	if owed := owedErasures(t, store); owed != 1 {
		t.Fatalf("owed erasures after the failed refresh = %d, want 1", owed)
	}
	assertOwedRowsHoldNoText(t, store, backupSecret)

	if err := store.RetryOwedErasure(); err != nil {
		t.Fatalf("RetryOwedErasure: %v", err)
	}
	assertBackupHolds(t, path, backupSecret, false)
	if owed := owedErasures(t, store); owed != 0 {
		t.Errorf("owed erasures after the retry = %d, want 0", owed)
	}
}

// TestARetryThatCannotRefreshTheBackupErasesIt fails if a retry whose refresh
// fails again keeps the backup that holds the deleted words, or removes it
// without overwriting its bytes.
func TestARetryThatCannotRefreshTheBackupErasesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	held, size := holdFile(t, preMigrationBackupPath(path))
	failVacuumInto(t, 2)
	segments, err := store.GetSegments("backed-up")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("backed-up", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); !errors.Is(err, ErrSegmentPostCommitMaintenance) {
		t.Fatalf("DeleteSegment with a full disk = %v", err)
	}

	if err := store.RetryOwedErasure(); err != nil {
		t.Fatalf("RetryOwedErasure: %v", err)
	}
	if _, err := os.Stat(preMigrationBackupPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the backup that could not be refreshed is still there (%v)", err)
	}
	assertHeldZeroed(t, held, size, "backup the retry removed")
	if owed := owedErasures(t, store); owed != 0 {
		t.Errorf("owed erasures after the retry = %d, want 0", owed)
	}
}

// TestAFailedLogOverwriteIsRetried fails if a deletion whose write-ahead log
// overwrite fails leaves the old frames, deleted words included, for the next
// truncation to free: a retry must overwrite the log first.
func TestAFailedLogOverwriteIsRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, _ = segmentTestSession(t, store, "logged")
	segments, err := store.GetSegments("logged")
	if err != nil {
		t.Fatal(err)
	}
	maintenance := postDeleteMaintenance
	t.Cleanup(func() { postDeleteMaintenance = maintenance })
	postDeleteMaintenance = func(*sql.DB, string) error { return errors.New("a reader kept the log from starting over") }
	if err := store.DeleteSegment("logged", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); !errors.Is(err, ErrSegmentPostCommitMaintenance) {
		t.Fatalf("DeleteSegment with a failing log overwrite = %v", err)
	}
	if log, err := os.ReadFile(path + "-wal"); err != nil || !bytes.Contains(log, []byte("private deletion token")) {
		t.Fatalf("control: the log does not hold the words after the failed overwrite (%v)", err)
	}
	postDeleteMaintenance = maintenance
	truncate := truncateWriteAheadLog
	t.Cleanup(func() { truncateWriteAheadLog = truncate })
	checked := false
	truncateWriteAheadLog = func(db *sql.DB) error {
		log, err := os.ReadFile(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		checked = true
		if bytes.Contains(log, []byte("private deletion token")) {
			t.Error("the retry truncates the log while it still holds the deleted words")
		}
		return truncate(db)
	}

	if err := store.RetryOwedErasure(); err != nil {
		t.Fatalf("RetryOwedErasure: %v", err)
	}
	if !checked {
		t.Fatal("the retry never truncated the log")
	}
	if owed := owedErasures(t, store); owed != 0 {
		t.Errorf("owed erasures after the retry = %d, want 0", owed)
	}
}

// TestOpeningTheStoreRetriesAnOwedErasure fails if an erase owed when the app
// stopped is not settled as the app starts again.
func TestOpeningTheStoreRetriesAnOwedErasure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	failVacuumInto(t, 1)
	segments, err := store.GetSegments("backed-up")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("backed-up", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); !errors.Is(err, ErrSegmentPostCommitMaintenance) {
		t.Fatalf("DeleteSegment with a full disk = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertBackupHolds(t, path, backupSecret, false)
	if owed := owedErasures(t, reopened); owed != 0 {
		t.Errorf("owed erasures after the app started again = %d, want 0", owed)
	}
}

// TestABackupWithoutSegmentIDsLosesTheDeletedWords fails if the scrub of a
// backup whose segments table has no id column leaves the deleted segment in
// it, or writes its words back into the canonical transcript it rebuilds
// from the segments it could not delete from.
func TestABackupWithoutSegmentIDsLosesTheDeletedWords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	backup, err := sql.Open("sqlite", preMigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE segments_without_ids AS SELECT session_id, speaker, text, start_time, end_time, timestamp FROM segments`,
		`DROP TABLE segments`,
		`ALTER TABLE segments_without_ids RENAME TO segments`,
		`VACUUM`,
	} {
		if _, err := backup.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := backup.Close(); err != nil {
		t.Fatal(err)
	}
	assertBackupHolds(t, path, backupSecret, true)

	deleteBackedUpSecret(t, store)
	assertBackupHolds(t, path, backupSecret, false)
	check, err := sql.Open("sqlite", preMigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = check.Close() }()
	var kept int
	if err := check.QueryRow(`SELECT count(*) FROM segments WHERE session_id = 'backed-up'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 2 {
		t.Errorf("the backup keeps %d of the session's segments, want the two not deleted", kept)
	}
	var canonical string
	if err := check.QueryRow(`SELECT canonical_transcript FROM sessions WHERE id = 'backed-up'`).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if canonical != "" {
		t.Errorf("backup canonical transcript = %q, want it cleared: with no segment ids a rebuild cannot be sure the deleted line is gone", canonical)
	}
}

// TestALeftoverScratchTableStaysOutOfTheBackup fails if the scratch table a
// stopped write-ahead log overwrite left in the database is still there when
// the next start takes the backup: the backup would carry it and never again
// have the database's schema, so every later deletion would scrub it in place
// rather than take it again.
func TestALeftoverScratchTableStaysOutOfTheBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`CREATE TABLE ` + walScratchTable + ` (zeros BLOB)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	backup, err := sql.Open("sqlite", preMigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backup.Close() }()
	var carried int
	if err := backup.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, walScratchTable).Scan(&carried); err != nil {
		t.Fatal(err)
	}
	if carried != 0 {
		t.Error("the backup taken at the next start carries the leftover scratch table")
	}
	if same, err := reopened.backupHasDatabaseSchema(preMigrationBackupPath(path)); err != nil || !same {
		t.Errorf("backup has the database's schema = %v (%v), want true", same, err)
	}
}

// TestTheBackupIsOpenedWithTemporaryDataInMemory fails if a connection to the
// backup can spill a sort or the search index rebuild over its transcripts to
// a temporary file, or frees pages without zeroing them.
func TestTheBackupIsOpenedWithTemporaryDataInMemory(t *testing.T) {
	backup, err := sql.Open("sqlite", backupDSN(filepath.Join(t.TempDir(), "backup.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backup.Close() }()
	var tempStore, secureDelete int
	if err := backup.QueryRow(`PRAGMA temp_store`).Scan(&tempStore); err != nil {
		t.Fatal(err)
	}
	if err := backup.QueryRow(`PRAGMA secure_delete`).Scan(&secureDelete); err != nil {
		t.Fatal(err)
	}
	if tempStore != 2 || secureDelete != 1 {
		t.Errorf("backup connection has temp_store=%d secure_delete=%d, want 2 and 1", tempStore, secureDelete)
	}
}

// deleteSecretOwingTheErase deletes the segment holding backupSecret while
// its erase cannot finish, and checks the erase is owed.
func deleteSecretOwingTheErase(t *testing.T, store *SQLiteStore) {
	t.Helper()
	segments, err := store.GetSegments("backed-up")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("backed-up", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); !errors.Is(err, ErrSegmentPostCommitMaintenance) {
		t.Fatalf("DeleteSegment whose erase cannot finish = %v, want %v", err, ErrSegmentPostCommitMaintenance)
	}
	if owed := owedErasures(t, store); owed != 1 {
		t.Fatalf("control: owed erasures after the failed erase = %d, want 1", owed)
	}
}

// migrateAfterTheBackup gives the database a table the backup lacks, as a
// migration at this start does: the backup keeps the schema before it.
func migrateAfterTheBackup(t *testing.T, store *SQLiteStore, path string) {
	t.Helper()
	if _, err := store.DB().Exec(`CREATE TABLE newer_schema (x)`); err != nil {
		t.Fatal(err)
	}
	if same, err := store.backupHasDatabaseSchema(preMigrationBackupPath(path)); err != nil || same {
		t.Fatalf("control: backup has the database's schema = %v (%v), want false", same, err)
	}
}

// TestARetryErasesABackupOfAnOlderSchema fails if a retry keeps, or removes
// without overwriting, a backup in an older schema than the database's (after
// a migration at this start). The deleted text is not kept, so the backup
// cannot be scrubbed, and it holds the deleted words.
func TestARetryErasesABackupOfAnOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	held, size := holdFile(t, preMigrationBackupPath(path))
	failVacuumInto(t, 1)
	deleteSecretOwingTheErase(t, store)
	migrateAfterTheBackup(t, store, path)

	if err := store.RetryOwedErasure(); err != nil {
		t.Fatalf("RetryOwedErasure: %v", err)
	}
	if _, err := os.Stat(preMigrationBackupPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the backup of an older schema is still there (%v)", err)
	}
	assertHeldZeroed(t, held, size, "backup of an older schema")
	if owed := owedErasures(t, store); owed != 0 {
		t.Errorf("owed erasures after the retry = %d, want 0", owed)
	}
}

// TestADeletionFinishesABackupEraseLeftPartWay fails if a deletion's own
// erase leaves, beside the database, a backup that an earlier erase moved off
// its name but could not overwrite (erase.ErrLeftover): its bytes, deleted
// words included, would stay until the next start.
func TestADeletionFinishesABackupEraseLeftPartWay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	// What erase.Replace leaves when it cannot overwrite the backup it
	// replaced: the old backup under a hidden name no other name links.
	contents, err := os.ReadFile(preMigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(filepath.Dir(path), ".erasing-0123456789abcdef01234567")
	if err := os.WriteFile(leftover, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	held, size := holdFile(t, leftover)

	deleteBackedUpSecret(t, store)
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the backup erase left part way is still there (%v)", err)
	}
	assertHeldZeroed(t, held, size, "backup erase left part way")
}

// TestALaterDeletionFinishesAnEarlierOwedErase fails if a deletion made
// while an earlier deletion's erase is owed clears it with an erase of its
// own words only: a scrub of a backup in an older schema takes out only the
// later deletion's words, and the earlier one's would stay with nothing owed.
func TestALaterDeletionFinishesAnEarlierOwedErase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	migrateAfterTheBackup(t, store, path)
	maintenance := postDeleteMaintenance
	t.Cleanup(func() { postDeleteMaintenance = maintenance })
	postDeleteMaintenance = func(*sql.DB, string) error { return errors.New("a reader kept the log from starting over") }
	deleteSecretOwingTheErase(t, store)
	assertBackupHolds(t, path, backupSecret, true)
	postDeleteMaintenance = maintenance

	segments, err := store.GetSegments("backed-up")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("backed-up", segments[0].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 0, End: 2, SHA256Before: "c", SHA256After: "d"}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	if owed := owedErasures(t, store); owed != 0 {
		t.Errorf("owed erasures after the later deletion = %d, want 0", owed)
	}
	backup := preMigrationBackupPath(path)
	for _, file := range []string{backup, backup + "-wal"} {
		contents, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(contents, []byte(backupSecret)) {
			t.Errorf("%s still holds the earlier deletion's words", filepath.Base(file))
		}
	}
}

// TestARetryKeepsAnEraseOwedSinceItRead fails if a retry clears the owed
// erase although a later deletion recorded one after the retry read it (a
// deletion commits while the retry erases, and waits for eraseMu only for its
// own erase): that deletion's erase would be lost if its own then failed.
func TestARetryKeepsAnEraseOwedSinceItRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	failVacuumInto(t, 1)
	deleteSecretOwingTheErase(t, store)
	maintenance := postDeleteMaintenance
	t.Cleanup(func() { postDeleteMaintenance = maintenance })
	postDeleteMaintenance = func(db *sql.DB, walPath string) error {
		// A later deletion commits: it takes the next number and records it.
		if _, err := db.Exec(`UPDATE app_state SET value = CAST(value AS INTEGER) + 1 WHERE key = ?`, owedEraseSeqKey); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE app_state SET value = (SELECT value FROM app_state WHERE key = ?) WHERE key = ?`, owedEraseSeqKey, owedEraseKey); err != nil {
			return err
		}
		return maintenance(db, walPath)
	}

	if err := store.RetryOwedErasure(); err != nil {
		t.Fatalf("RetryOwedErasure: %v", err)
	}
	if owed := owedErasures(t, store); owed != 2 {
		t.Errorf("owed erasures after a retry a deletion overtook = %d, want 2", owed)
	}
}

// TestARetryWhoseLogOverwriteFailsStaysOwed fails if a retry whose own
// overwrite of the write-ahead log fails records the erase done: the log's
// old frames would keep the deleted words with nothing owed.
func TestARetryWhoseLogOverwriteFailsStaysOwed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	maintenance := postDeleteMaintenance
	t.Cleanup(func() { postDeleteMaintenance = maintenance })
	postDeleteMaintenance = func(*sql.DB, string) error { return errors.New("a reader kept the log from starting over") }
	deleteSecretOwingTheErase(t, store)

	if err := store.RetryOwedErasure(); err == nil {
		t.Error("RetryOwedErasure with a failing log overwrite reported no error")
	}
	if owed := owedErasures(t, store); owed != 1 {
		t.Errorf("owed erasures after a retry whose log overwrite failed = %d, want 1", owed)
	}
}
