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

// owedErasures counts the deletions whose erase from the write-ahead log and
// the backup is still owed.
func owedErasures(t *testing.T, store *SQLiteStore) int {
	t.Helper()
	var owed int
	if err := store.DB().QueryRow(`SELECT count(*) FROM deleted_segments WHERE erase_owed = 1`).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	return owed
}

// assertOwedRowsHoldNoText fails if a row that records an owed erase holds
// any of the deleted words.
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
