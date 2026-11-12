package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// preMigrationBackupPath is the backup createPreMigrationBackup keeps beside
// the database at dbPath.
func preMigrationBackupPath(dbPath string) string {
	return dbPath + ".pre-migrate.bak"
}

// backupDSN is the name the backup at backupPath is opened with:
// secure_delete zeroes the pages a scrub frees, and temporary data stays in
// memory, so a sort or the search index rebuild over the backup's transcripts
// leaves no text in the blocks a temporary file frees.
func backupDSN(backupPath string) string {
	return backupPath + "?_pragma=secure_delete(1)&_pragma=temp_store(2)&_pragma=busy_timeout(5000)"
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
	backup, err := sql.Open("sqlite", backupDSN(backupPath))
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

// commitBackupScrub commits the scrub of the backup (tx). Tests replace it to
// look at the files beside the backup while the scrub is open.
var commitBackupScrub = func(tx *sql.Tx) error { return tx.Commit() }

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
// own schema. It works through a write-ahead log, not the rollback journal a
// backup made by VACUUM INTO has: a rollback journal holds each page as it
// was before the change, the deleted words included, and is unlinked at the
// commit, leaving them in blocks the file system frees; a write-ahead log
// holds only the pages as they are after it, with secure_delete's zeros. The
// backup is left in the journal mode it had, with no log beside it.
func scrubBackup(backup *sql.DB, segmentID int64, deletedText string, sessionIDs, holderIDs []string, span *DeletedSpan) (err error) {
	var mode string
	if err := backup.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("read backup journal mode: %w", err)
	}
	if _, err := backup.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("give the backup a write-ahead log: %w", err)
	}
	defer func() {
		// Back to its mode, the log checkpointed and removed.
		if _, restoreErr := backup.Exec(`PRAGMA journal_mode = ` + mode); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore backup journal mode %s: %w", mode, restoreErr))
		}
	}()
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
	// rebuildable: the deleted segment was found by its id, so the remaining
	// segments are the ones the canonical transcript may be rebuilt from.
	rebuildable := false
	switch {
	case segments["id"]:
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
		rebuildable = true
	case segments["text"]:
		// With no id, the segment is found by its text, in the sessions that
		// can hold it: every segment there with that text goes, an identical
		// line kept elsewhere in those sessions with it.
		query, args := `DELETE FROM segments WHERE text = ?`, []any{deletedText}
		if segments["session_id"] {
			query += ` AND session_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(affected)), ",") + `)`
			for _, id := range affected {
				args = append(args, id)
			}
		}
		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf("delete the segment from the backup by its text: %w", err)
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
	// Each affected session's canonical transcript is rebuilt from the
	// backup's own remaining segments, in its order, as this version writes
	// one; it is cleared when the schema has no segments to rebuild it from,
	// or when the deleted segment could not be found by its id (a rebuild could
	// write its words back). Nothing depends on how the build that made the
	// backup formatted it.
	if sessions["canonical_transcript"] {
		rebuildFrom := segments
		if !rebuildable {
			rebuildFrom = nil
		}
		if err := rebuildBackupCanonical(tx, rebuildFrom, affected); err != nil {
			return err
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
	if err := commitBackupScrub(tx); err != nil {
		return fmt.Errorf("commit backup scrub: %w", err)
	}
	return nil
}

// rebuildBackupCanonical sets the canonical transcript of each session in
// sessionIDs from the backup's remaining segments of that session, in the
// order getSegments reads them (by id), when its segments table has the
// session and text columns that needs (segmentColumns), and clears it when
// it does not.
func rebuildBackupCanonical(tx *sql.Tx, segmentColumns map[string]bool, sessionIDs []string) error {
	for _, id := range sessionIDs {
		canonical := ""
		if segmentColumns["session_id"] && segmentColumns["text"] {
			order := "rowid"
			if segmentColumns["id"] {
				order = "id"
			}
			rows, err := tx.Query(`SELECT text FROM segments WHERE session_id = ? ORDER BY `+order, id)
			if err != nil {
				return fmt.Errorf("read backup segments of session %s: %w", id, err)
			}
			var remaining []transcribe.Segment
			for rows.Next() {
				var segment transcribe.Segment
				if err := rows.Scan(&segment.Text); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan backup segment of session %s: %w", id, err)
				}
				remaining = append(remaining, segment)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iterate backup segments of session %s: %w", id, err)
			}
			_ = rows.Close()
			canonical = canonicalTranscript(remaining)
		}
		if _, err := tx.Exec(`UPDATE sessions SET canonical_transcript = ? WHERE id = ?`, canonical, id); err != nil {
			return fmt.Errorf("rebuild backup canonical transcript of session %s: %w", id, err)
		}
	}
	return nil
}
