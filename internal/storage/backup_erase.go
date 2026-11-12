package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// preMigrationBackupPath is the backup createPreMigrationBackup keeps beside
// the database at dbPath.
func preMigrationBackupPath(dbPath string) string {
	return dbPath + ".pre-migrate.bak"
}

// eraseFromBackup removes a committed segment deletion from the pre-migration
// backup (createPreMigrationBackup), which holds the database as this start
// found it, the deleted words included.
//
// The backup exists so the previous build can start again after a rollback.
// If no migration ran at this start, the backup and the database have the
// same schema, and the backup is taken again from the database through the
// same VACUUM INTO path: the words are not in it. If a migration ran, the
// backup is the only copy in the old schema, so it is kept and scrubbed in
// place instead: opened with secure_delete on, the deleted segment's row
// removed, the words taken out of the rows, events and search index that hold
// them in whatever of those tables and columns its schema has, and its
// write-ahead log checkpointed away.
//
// segmentID and deletedText name the deleted segment; sessionIDs are the
// sessions it was deleted from or merged into it, and holderIDs those that
// held it (see rewriteDeletedTextEvents).
func (s *SQLiteStore) eraseFromBackup(segmentID int64, deletedText string, sessionIDs, holderIDs []string, span *DeletedSpan) error {
	if s.path == "" {
		return nil
	}
	backupPath := preMigrationBackupPath(s.path)
	if _, err := os.Stat(backupPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat backup: %w", err)
	}
	backup, err := sql.Open("sqlite", backupPath+"?_pragma=secure_delete(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	backup.SetMaxOpenConns(1)
	same, err := sameSchema(s.db, backup)
	if err != nil {
		_ = backup.Close()
		return err
	}
	if same {
		if err := backup.Close(); err != nil {
			return fmt.Errorf("close backup: %w", err)
		}
		return createPreMigrationBackup(s.db, s.path)
	}
	scrubErr := scrubBackup(backup, segmentID, deletedText, sessionIDs, holderIDs, span)
	if err := backup.Close(); err != nil && scrubErr == nil {
		return fmt.Errorf("close backup: %w", err)
	}
	return scrubErr
}

// sameSchema reports whether databases a and b have the same schema: the same
// tables, indexes and triggers, defined alike, and the same data migrations
// recorded (runOnce), so that one written in the other's place loses nothing
// the previous build needs.
func sameSchema(a, b *sql.DB) (bool, error) {
	describe := func(db *sql.DB) (string, error) {
		rows, err := db.Query(`SELECT type || ' ' || name || ' ' || coalesce(sql, '') FROM sqlite_master
			WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
		if err != nil {
			return "", fmt.Errorf("read schema: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var entries []string
		for rows.Next() {
			var entry string
			if err := rows.Scan(&entry); err != nil {
				return "", fmt.Errorf("scan schema: %w", err)
			}
			entries = append(entries, entry)
		}
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("iterate schema: %w", err)
		}
		if slices.ContainsFunc(entries, func(entry string) bool { return strings.HasPrefix(entry, "table migrations ") }) {
			migrations, err := db.Query(`SELECT name FROM migrations ORDER BY name`)
			if err != nil {
				return "", fmt.Errorf("read migrations: %w", err)
			}
			defer func() { _ = migrations.Close() }()
			for migrations.Next() {
				var name string
				if err := migrations.Scan(&name); err != nil {
					return "", fmt.Errorf("scan migration: %w", err)
				}
				entries = append(entries, "migration "+name)
			}
			if err := migrations.Err(); err != nil {
				return "", fmt.Errorf("iterate migrations: %w", err)
			}
		}
		return strings.Join(entries, "\n"), nil
	}
	current, err := describe(a)
	if err != nil {
		return false, err
	}
	backup, err := describe(b)
	if err != nil {
		return false, fmt.Errorf("backup: %w", err)
	}
	return current == backup, nil
}

// scrubBackup removes the deleted segment from the backup at backup, in its
// own schema, and checkpoints its write-ahead log away.
func scrubBackup(backup *sql.DB, segmentID int64, deletedText string, sessionIDs, holderIDs []string, span *DeletedSpan) error {
	tx, err := backup.Begin()
	if err != nil {
		return fmt.Errorf("begin scrubbing backup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	columns := func(table string) (map[string]bool, error) {
		rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return nil, fmt.Errorf("read backup table %s: %w", table, err)
		}
		defer func() { _ = rows.Close() }()
		found := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, fmt.Errorf("scan backup table %s: %w", table, err)
			}
			found[name] = true
		}
		return found, rows.Err()
	}

	// The backup may predate a merge: the session that held the segment then
	// is the one its row still names there.
	affected := slices.Clone(sessionIDs)
	holders := slices.Clone(holderIDs)
	segments, err := columns("segments")
	if err != nil {
		return err
	}
	if segments["id"] {
		var holder string
		err := tx.QueryRow(`SELECT session_id FROM segments WHERE id = ?`, segmentID).Scan(&holder)
		switch {
		case err == nil:
			affected = append(affected, holder)
			holders = append(holders, holder)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("find the deleted segment in the backup: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM segments WHERE id = ?`, segmentID); err != nil {
			return fmt.Errorf("delete the segment from the backup: %w", err)
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(affected)), ",")
	args := make([]any, len(affected))
	for i, id := range affected {
		args[i] = id
	}
	sessions, err := columns("sessions")
	if err != nil {
		return err
	}
	for _, column := range []string{"title", "summary", "refined_transcript"} {
		if sessions[column] {
			if _, err := tx.Exec(`UPDATE sessions SET `+column+` = '' WHERE id IN (`+placeholders+`)`, args...); err != nil {
				return fmt.Errorf("clear backup sessions %s: %w", column, err)
			}
		}
	}
	if line := strings.TrimSpace(deletedText); sessions["canonical_transcript"] && line != "" {
		if _, err := tx.Exec(`UPDATE sessions SET canonical_transcript = replace(canonical_transcript, ?, '') WHERE id IN (`+placeholders+`)`,
			append([]any{line}, args...)...); err != nil {
			return fmt.Errorf("scrub backup canonical transcripts: %w", err)
		}
	}
	if embeddings, err := columns("embeddings"); err != nil {
		return err
	} else if embeddings["session_id"] {
		if _, err := tx.Exec(`DELETE FROM embeddings WHERE session_id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("clear backup embeddings: %w", err)
		}
	}
	if events, err := columns("mcp_events"); err != nil {
		return err
	} else if events["payload"] {
		if err := rewriteDeletedTextEvents(tx, affected, holders, deletedText, span); err != nil {
			return fmt.Errorf("rewrite backup events: %w", err)
		}
	}
	if search, err := columns("sessions_fts"); err != nil {
		return err
	} else if len(search) != 0 {
		if _, err := tx.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("rebuild backup search index: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit backup scrub: %w", err)
	}
	if _, err := backup.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint backup: %w", err)
	}
	return nil
}
