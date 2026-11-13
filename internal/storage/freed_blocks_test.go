package storage

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// holdFile opens the file at path, so the test still reads its inode after
// the name is gone or replaced.
func holdFile(t *testing.T, path string) (*os.File, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return f, int(info.Size())
}

// assertHeldZeroed fails unless the file held reads size zero bytes.
func assertHeldZeroed(t *testing.T, held *os.File, size int, what string) {
	t.Helper()
	got := make([]byte, size)
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatalf("read the %s: %v", what, err)
	}
	if !bytes.Equal(got, make([]byte, size)) {
		t.Errorf("the %s still holds its bytes (backupSecret found: %v)", what, bytes.Contains(got, []byte(backupSecret)))
	}
}

// TestTheWriteAheadLogIsOverwrittenBeforeItIsTruncated fails if a deletion
// truncates the write-ahead log while older frames in it still hold the
// deleted words: truncation frees their blocks with the words in them.
func TestTheWriteAheadLogIsOverwrittenBeforeItIsTruncated(t *testing.T) {
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
	if log, err := os.ReadFile(path + "-wal"); err != nil || !bytes.Contains(log, []byte("private deletion token")) {
		t.Fatalf("control: the write-ahead log does not hold the words before the deletion (%v)", err)
	}
	truncate := truncateWriteAheadLog
	t.Cleanup(func() { truncateWriteAheadLog = truncate })
	checked := false
	truncateWriteAheadLog = func(db *sql.DB) error {
		log, err := os.ReadFile(path + "-wal")
		if err != nil {
			t.Fatalf("read the write-ahead log before it is truncated: %v", err)
		}
		checked = true
		if bytes.Contains(log, []byte("private deletion token")) {
			t.Error("the write-ahead log still holds the deleted words when it is truncated")
		}
		return truncate(db)
	}

	if err := deleteRefinedAudioTestSegment(t, store, "logged", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatalf("delete refined segment: %v", err)
	}
	if !checked {
		t.Fatal("the deletion never truncated the write-ahead log")
	}
	var scratch int
	if err := store.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, walScratchTable).Scan(&scratch); err != nil || scratch != 0 {
		t.Errorf("the scratch table is left (%d, %v)", scratch, err)
	}
	assertDatabaseFilesLack(t, path, "private deletion token", "after bridge")
}

// TestARefreshedBackupZeroesTheBackupItReplaces fails if a deletion takes the
// backup again without overwriting the backup it replaces, which held the
// deleted words, before the file system frees it.
func TestARefreshedBackupZeroesTheBackupItReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	held, size := holdFile(t, preMigrationBackupPath(path))
	deleteBackedUpSecret(t, store)
	assertBackupHolds(t, path, backupSecret, false)
	assertHeldZeroed(t, held, size, "backup the refresh replaced")
}

// TestABackupScrubRebuildsAFormattedCanonicalTranscript fails if the scrub of
// a backup of an older schema keeps the deleted words in a canonical
// transcript the build that made it formatted differently (it must not
// depend on the deleted text appearing as this version writes it).
func TestABackupScrubRebuildsAFormattedCanonicalTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE sessions SET canonical_transcript = '[00:00] Speaker 1: before bridge' || char(10) || '[00:02] Speaker 2: ` + backupSecret + `' || char(10) || '[00:04] Speaker 1: after bridge' WHERE id = 'backed-up'`,
		`ALTER TABLE sessions DROP COLUMN drive_replace_error`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := openStoreWithBackup(t, path)
	deleteBackedUpSecret(t, store)
	assertBackupHolds(t, path, backupSecret, false)
	backup, err := sql.Open("sqlite", preMigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backup.Close() }()
	var canonical string
	if err := backup.QueryRow(`SELECT canonical_transcript FROM sessions WHERE id = 'backed-up'`).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if canonical != "before bridge\nafter bridge\n" {
		t.Errorf("backup canonical transcript = %q, want it rebuilt from the remaining segments", canonical)
	}
}

// TestABackupScrubLeavesNoJournalHoldingTheWords fails if the scrub of a
// backup of an older schema goes through a rollback journal: it holds each
// page as it was, the deleted words included, and is unlinked at the commit,
// freeing blocks with the words in them.
func TestABackupScrubLeavesNoJournalHoldingTheWords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
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
	commit := commitBackupScrub
	t.Cleanup(func() { commitBackupScrub = commit })
	checked := false
	commitBackupScrub = func(tx *sql.Tx) error {
		backup := preMigrationBackupPath(path)
		for _, journal := range []string{backup + "-journal", backup + "-wal"} {
			contents, err := os.ReadFile(journal)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(contents), backupSecret) {
				t.Errorf("%s holds the deleted words while the scrub is open", filepath.Base(journal))
			}
		}
		checked = true
		return commit(tx)
	}

	store := openStoreWithBackup(t, path)
	deleteBackedUpSecret(t, store)
	if !checked {
		t.Fatal("the backup was not scrubbed in place")
	}
	assertBackupHolds(t, path, backupSecret, false)
	if _, err := os.Stat(preMigrationBackupPath(path) + "-wal"); !os.IsNotExist(err) {
		t.Errorf("the scrubbed backup has a write-ahead log beside it (%v)", err)
	}
}

// TestOpeningTheStoreFinishesALeftoverErasure fails if a backup the app
// stopped part way through erasing (.erasing-* beside the database) is left,
// or removed without its bytes overwritten.
func TestOpeningTheStoreFinishesALeftoverErasure(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, ".erasing-cut-off")
	if err := os.WriteFile(leftover, []byte(backupSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	held, size := holdFile(t, leftover)
	store, err := NewSQLiteStore(filepath.Join(dir, "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("the leftover is still there: %v", err)
	}
	assertHeldZeroed(t, held, size, "leftover erasure")
}
