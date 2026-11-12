package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/erase"
)

// RetryOwedErasure finishes the erase of every deletion whose erase from the
// write-ahead log and the pre-migration backup failed after it committed, or
// was cut off by the app stopping (deleted_segments.erase_owed): for example
// VACUUM INTO on a full disk. The segment is gone from the database, so the
// deletion itself cannot be run again, and its words would otherwise stay in
// the log's old frames and in the backup for good.
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
	owed, err := owedErasureRows(s.db)
	if err != nil || len(owed) == 0 {
		return err
	}
	if err := postDeleteMaintenance(s.db, s.path+"-wal"); err != nil {
		return fmt.Errorf("overwrite the write-ahead log: %w", err)
	}
	if !backupTaken {
		if err := s.refreshOrRemoveBackup(); err != nil {
			return err
		}
	}
	if err := s.eraseOrphans(); err != nil {
		return err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(owed)), ",")
	args := make([]any, len(owed))
	for i, row := range owed {
		args[i] = row
	}
	if _, err := s.db.Exec(`UPDATE deleted_segments SET erase_owed = 0 WHERE rowid IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("record the owed erase done: %w", err)
	}
	return nil
}

// owedErasureRows returns the deleted-span rows whose erase is owed.
func owedErasureRows(db *sql.DB) ([]int64, error) {
	rows, err := db.Query(`SELECT rowid FROM deleted_segments WHERE erase_owed = 1`)
	if err != nil {
		return nil, fmt.Errorf("list owed erasures: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var owed []int64
	for rows.Next() {
		var row int64
		if err := rows.Scan(&row); err != nil {
			return nil, fmt.Errorf("scan owed erasure: %w", err)
		}
		owed = append(owed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate owed erasures: %w", err)
	}
	return owed, nil
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
	if err := erase.Orphans(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("erase leftover backups: %w", err)
	}
	return nil
}
