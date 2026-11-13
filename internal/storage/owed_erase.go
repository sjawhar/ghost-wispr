package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/sjawhar/ghost-wispr/internal/erase"
)

// owedEraseKey is the app_state key whose row says that an erase of segment
// deletions from the write-ahead log and the pre-migration backup is owed. It
// holds the number (owedEraseSeqKey) of the last deletion that owes one, and
// the erase that covers them all removes it. It is one marker for the store,
// on no session's row, because that erase is store-wide (one log, one
// backup), and so that deleting a session does not take an owed erase with
// it.
const owedEraseKey = "segment_deletion_erase_owed"

// owedEraseSeqKey is the app_state key that numbers the segment deletions.
// It is never removed, so a number is never used twice: an erase that read an
// older number never clears the marker a later deletion wrote after the row
// had gone and come back.
const owedEraseSeqKey = "segment_deletion_erase_seq"

// RetryOwedErasure finishes the erase of every deletion whose erase from the
// write-ahead log and the pre-migration backup failed after it committed, or
// was cut off by the app stopping (owedEraseKey): for example VACUUM INTO on
// a full disk. The segment is gone from the database, so the deletion itself
// cannot be run again, and its words would otherwise stay in the log's old
// frames and in the backup for good.
//
// The deleted text is not kept, so the backup cannot be scrubbed again: it is
// taken again from the database when it has the database's schema, and erased
// when it does not or when taking it fails. The app runs it as it starts
// (NewSQLiteStore) and on every sync sweep.
func (s *SQLiteStore) RetryOwedErasure() error {
	return s.retryOwedErasure(false)
}

// retryOwedErasure is RetryOwedErasure. backupTaken: the backup was taken from
// the database at this start, after every owed deletion committed, so it holds
// none of their words and only the write-ahead log is left.
func (s *SQLiteStore) retryOwedErasure(backupTaken bool) error {
	s.eraseMu.Lock()
	defer s.eraseMu.Unlock()
	var owed string
	switch err := s.db.QueryRow(`SELECT value FROM app_state WHERE key = ?`, owedEraseKey).Scan(&owed); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("read owed erase: %w", err)
	}
	if err := s.eraseEveryOwed(backupTaken); err != nil {
		return err
	}
	return clearOwedErase(s.db, owed)
}

// eraseEveryOwed erases every committed deletion from the write-ahead log and,
// unless the backup was taken at this start (backupTaken), from the
// pre-migration backup, and finishes any backup erase left part way.
func (s *SQLiteStore) eraseEveryOwed(backupTaken bool) error {
	if err := postDeleteMaintenance(s.db, s.path+"-wal"); err != nil {
		return fmt.Errorf("overwrite the write-ahead log: %w", err)
	}
	if !backupTaken {
		if err := s.refreshOrRemoveBackup(); err != nil {
			return err
		}
	}
	return s.eraseOrphans()
}

// recordOwedErase records a deletion's owed erase, in its transaction: it
// takes the next deletion number (owedEraseSeqKey) and writes it to the owed
// marker (owedEraseKey). It returns that number, and whether an earlier
// deletion's erase was owed already.
func recordOwedErase(tx *sql.Tx) (owed string, earlier bool, err error) {
	switch err := tx.QueryRow(`SELECT 1 FROM app_state WHERE key = ?`, owedEraseKey).Scan(new(int)); {
	case err == nil:
		earlier = true
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, fmt.Errorf("read owed erase: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO app_state(key, value) VALUES(?, '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + 1`, owedEraseSeqKey); err != nil {
		return "", false, fmt.Errorf("number the deletion: %w", err)
	}
	if err := tx.QueryRow(`SELECT value FROM app_state WHERE key = ?`, owedEraseSeqKey).Scan(&owed); err != nil {
		return "", false, fmt.Errorf("read the deletion number: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO app_state(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, owedEraseKey, owed); err != nil {
		return "", false, fmt.Errorf("record owed erase: %w", err)
	}
	return owed, earlier, nil
}

// clearOwedErase records the owed erase done, unless a later deletion has
// recorded one since the erase read the number owed.
func clearOwedErase(db *sql.DB, owed string) error {
	if _, err := db.Exec(`DELETE FROM app_state WHERE key = ? AND value = ?`, owedEraseKey, owed); err != nil {
		return fmt.Errorf("record the owed erase done: %w", err)
	}
	return nil
}

// refreshOrRemoveBackup takes the pre-migration backup again from the
// database when it has the database's schema. Otherwise (an older schema
// cannot be scrubbed without the deleted text, which is not kept), or when
// taking it fails, it erases the backup: a backup that may hold deleted words
// is worse than none.
func (s *SQLiteStore) refreshOrRemoveBackup() error {
	if s.path == "" {
		return nil
	}
	backupPath := preMigrationBackupPath(s.path)
	if _, err := os.Stat(backupPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat backup: %w", err)
	}
	same, err := s.backupHasDatabaseSchema(backupPath)
	switch {
	case err != nil:
		slog.Warn("could not read the pre-migration backup; erasing it", "path", backupPath, "error", err)
	case same:
		refreshErr := createPreMigrationBackup(s.db, s.path)
		if refreshErr == nil {
			return nil
		}
		slog.Warn("could not take the pre-migration backup again; erasing it", "path", backupPath, "error", refreshErr)
	default:
		slog.Warn("the pre-migration backup has an older schema and cannot be scrubbed without the deleted text; erasing it", "path", backupPath)
	}
	for _, file := range []string{backupPath + "-wal", backupPath + "-shm", backupPath} {
		// A file gone from its name whose bytes are not overwritten yet is
		// finished by eraseOrphans.
		if err := erase.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, erase.ErrLeftover) {
			return fmt.Errorf("erase backup %s: %w", file, err)
		}
	}
	return nil
}

// backupHasDatabaseSchema reports whether the backup at backupPath has the
// database's schema (sameSchema).
func (s *SQLiteStore) backupHasDatabaseSchema(backupPath string) (bool, error) {
	backup, err := sql.Open("sqlite", backupDSN(backupPath))
	if err != nil {
		return false, fmt.Errorf("open backup: %w", err)
	}
	backup.SetMaxOpenConns(1)
	same, err := sameSchema(s.db, backup)
	if closeErr := backup.Close(); closeErr != nil && err == nil {
		err = fmt.Errorf("close backup: %w", closeErr)
	}
	return same, err
}

// eraseOrphans finishes, beside the database, an erase of a backup that was
// gone from its name but not yet overwritten (erase.ErrLeftover).
func (s *SQLiteStore) eraseOrphans() error {
	if s.path == "" {
		return nil
	}
	reserved, err := s.ReservedRawErasePaths()
	if err != nil {
		return fmt.Errorf("list reserved raw erases: %w", err)
	}
	if err := erase.OrphansExcept(filepath.Dir(s.path), func(path string) bool { return reserved[path] }); err != nil {
		return fmt.Errorf("erase leftover backups: %w", err)
	}
	return nil
}
