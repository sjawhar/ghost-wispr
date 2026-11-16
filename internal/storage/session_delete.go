package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/recording"
)

// deletedSessionEventsMigration names the one-time removal, as the app
// starts, of the stored events of sessions deleted before a session delete
// removed its events (dropEventsOfDeletedSessions).
const deletedSessionEventsMigration = "drop-events-of-deleted-sessions"

// DeleteSession deletes session id and its merged-source set, refusing a target
// that feeds another merge (ErrDeleteMergeActive), any active member, or any
// member that owes Google Drive a replacement
// (ErrDeleteReplacementPending). It returns the target row's audio path: the
// recordings the caller then erases are the ones the row named when it went,
// not ones it read before. In the same transaction, before the rows go, it
// records the directory of each of those recordings, found where
// recording.Resolve finds it in audioDir (the one the caller then erases), so
// an erase a stop cuts off is finished as the app starts; it deletes nothing
// if it cannot record one. In that transaction too it deletes every stored
// event that names a session it deletes (dropSessionSetEvents): a live
// transcript event holds the conversation's words and a summary event its
// summary. Once the delete commits, it erases it from the write-ahead log and
// the pre-migration backup, as a segment deletion does
// (eraseAfterSessionDeletion).
func (s *SQLiteStore) DeleteSession(id, audioDir string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin deleting session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var audioPath string
	switch err := tx.QueryRow(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) SELECT target.audio_path
		FROM sessions AS target
		WHERE target.id = ?
		  AND (target.merged_into = '' OR NOT EXISTS (
			SELECT 1 FROM sessions AS parent WHERE parent.id = target.merged_into
		  ))
		  AND NOT EXISTS (
			SELECT 1 FROM sessions AS member JOIN doomed ON member.id = doomed.id
			WHERE member.status = ?
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM sessions AS member JOIN doomed ON member.id = doomed.id
			WHERE member.drive_replace_pending = 1
			   OR EXISTS (
				SELECT 1 FROM drive_replacement_folders
				WHERE drive_replacement_folders.folder_session_id = member.id
				  AND drive_replacement_folders.state = ?
			   )
		  )`,
		id, id, SessionActive, DriveReplacementFolderOwed).Scan(&audioPath); {
	case errors.Is(err, sql.ErrNoRows):
		var exists int
		switch err := tx.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&exists); {
		case errors.Is(err, sql.ErrNoRows):
			return "", sql.ErrNoRows
		case err != nil:
			return "", fmt.Errorf("read session %s to delete it: %w", id, err)
		}
		var unsafe int
		err := tx.QueryRow(`WITH RECURSIVE doomed(id) AS (
				SELECT ?
				UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
			) SELECT 1
			WHERE EXISTS (
				SELECT 1 FROM sessions AS target
				JOIN sessions AS parent ON parent.id = target.merged_into
				WHERE target.id = ?
			)
			   OR EXISTS (
				SELECT 1 FROM sessions AS member JOIN doomed ON member.id = doomed.id
				WHERE member.status = ?
			   )`,
			id, id, SessionActive).Scan(&unsafe)
		switch {
		case err == nil:
			return "", fmt.Errorf("delete session %s: %w", id, ErrDeleteMergeActive)
		case errors.Is(err, sql.ErrNoRows):
			return "", fmt.Errorf("delete session %s: %w", id, ErrDeleteReplacementPending)
		default:
			return "", fmt.Errorf("check whether session %s is safe to delete: %w", id, err)
		}
	case err != nil:
		return "", fmt.Errorf("read session %s to delete it: %w", id, err)
	}
	rows, err := tx.Query(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) SELECT sessions.id, sessions.audio_path FROM sessions JOIN doomed ON sessions.id = doomed.id`, id)
	if err != nil {
		return "", fmt.Errorf("list merged sources of session %s: %w", id, err)
	}
	paths := []string{audioPath}
	deletedIDs := []string{id}
	for rows.Next() {
		var sourceID, sourceAudioPath string
		if err := rows.Scan(&sourceID, &sourceAudioPath); err != nil {
			_ = rows.Close()
			return "", fmt.Errorf("scan merged source of session %s: %w", id, err)
		}
		if sourceID == id {
			continue
		}
		deletedIDs = append(deletedIDs, sourceID)
		paths = append(paths, sourceAudioPath, recording.RawPath(audioDir, sourceID))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", fmt.Errorf("list merged sources of session %s: %w", id, err)
	}
	audioPath = strings.Join(paths, ",")
	for _, entry := range append(recording.Entries(audioPath), recording.RawPath(audioDir, id)) {
		if err := recordEraseDirectory(tx, filepath.Dir(recording.Resolve(entry, audioDir))); err != nil {
			return "", fmt.Errorf("delete session %s: %w", id, err)
		}
	}
	if _, err := tx.Exec(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) DELETE FROM raw_erase_reservations WHERE session_id IN (SELECT id FROM doomed)`, id); err != nil {
		return "", fmt.Errorf("clear raw erase reservations for session %s: %w", id, err)
	}
	if err := dropSessionSetEvents(tx, id); err != nil {
		return "", err
	}
	owed, earlierOwed, err := recordOwedErase(tx)
	if err != nil {
		return "", fmt.Errorf("delete session %s: %w", id, err)
	}
	if _, err := tx.Exec(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) DELETE FROM sessions WHERE id IN (SELECT id FROM doomed)`, id); err != nil {
		return "", fmt.Errorf("delete session %s and its merged sources: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit deleting session %s: %w", id, err)
	}
	s.eraseAfterSessionDeletion(owed, earlierOwed, deletedIDs)
	return audioPath, nil
}

// dropSessionSetEvents deletes, in tx, every stored event that names session
// id or a session merged into it. It runs before their rows go, while the
// merged sources can still be found.
func dropSessionSetEvents(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) DELETE FROM mcp_events
		WHERE json_valid(payload) AND json_extract(payload, '$.session_id') IN (SELECT id FROM doomed)`, id); err != nil {
		return fmt.Errorf("delete the stored events of session %s and its merged sources: %w", id, err)
	}
	return nil
}

// dropEventsOfDeletedSessions deletes, in tx, every stored event that names a
// session the database does not have, and returns how many it deleted. Events
// that name no session (a pause, a component's status) stay.
func dropEventsOfDeletedSessions(tx *sql.Tx) (int64, error) {
	res, err := tx.Exec(`DELETE FROM mcp_events
		WHERE json_valid(payload)
		  AND COALESCE(json_extract(payload, '$.session_id'), '') != ''
		  AND NOT EXISTS (SELECT 1 FROM sessions WHERE sessions.id = json_extract(mcp_events.payload, '$.session_id'))`)
	if err != nil {
		return 0, fmt.Errorf("delete the stored events of deleted sessions: %w", err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the stored events of deleted sessions: %w", err)
	}
	return removed, nil
}

// dropEventsOfDeletedSessionsOnce deletes, once per database
// (deletedSessionEventsMigration), the stored events of sessions deleted
// before a session delete deleted its events (dropEventsOfDeletedSessions):
// /api/events served them for up to seven days. The app runs it as it starts,
// before it takes the pre-migration backup, so the backup holds none of them,
// and records their erase from the write-ahead log owed (recordOwedErase),
// which the start then finishes (retryOwedErasure). A database that does not
// yet have every table this needs is left for a later start, so nothing is
// created before the backup is taken.
func (s *SQLiteStore) dropEventsOfDeletedSessionsOnce() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin deleting the events of deleted sessions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"mcp_events", "sessions", "migrations", "app_state"} {
		if exists, err := tableExists(tx, table); err != nil {
			return fmt.Errorf("check table %s before deleting the events of deleted sessions: %w", table, err)
		} else if !exists {
			return nil
		}
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO migrations(name, applied_at) VALUES(?, ?)`, deletedSessionEventsMigration, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("record migration %s: %w", deletedSessionEventsMigration, err)
	}
	if added, err := res.RowsAffected(); err != nil || added == 0 {
		return err
	}
	removed, err := dropEventsOfDeletedSessions(tx)
	if err != nil {
		return err
	}
	if removed > 0 {
		if _, _, err := recordOwedErase(tx); err != nil {
			return fmt.Errorf("delete the events of deleted sessions: %w", err)
		}
		slog.Info("deleted the stored events of deleted sessions", "events", removed)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit deleting the events of deleted sessions: %w", err)
	}
	return nil
}

// eraseAfterSessionDeletion erases a committed session delete (sessionIDs:
// the session and its merged sources) from the write-ahead log and the
// pre-migration backup (scrubBackupSessions when the backup has an older
// schema), as eraseAfterDeletion does a segment deletion, and clears the owed
// erase its transaction recorded (owed) unless a later deletion has recorded
// one since. When an earlier deletion's erase was owed too (earlierOwed), it
// runs the erase a retry runs, which covers both. The delete itself is done,
// so a failure is logged, not returned: the erase stays owed, and
// RetryOwedErasure finishes it at the next start or sync sweep.
func (s *SQLiteStore) eraseAfterSessionDeletion(owed string, earlierOwed bool, sessionIDs []string) {
	s.eraseMu.Lock()
	defer s.eraseMu.Unlock()
	var err error
	if earlierOwed {
		err = s.eraseEveryOwed(false)
	} else {
		err = s.eraseDeletion(func(backup *sql.DB) error { return scrubBackupSessions(backup, sessionIDs) })
	}
	if err == nil {
		err = clearOwedErase(s.db, owed)
	}
	if err != nil {
		slog.Warn("a deleted session's erase from the database log or backup is still owed", "sessions", sessionIDs, "error", err)
	}
}

// DeleteEndedSessionAndSources deletes an age-eligible merged session and its
// source rows in one transaction, with every stored event that names one of
// them (dropSessionSetEvents). It records every raw capture directory
// before the rows go. It refuses when a session in that deletion set is
// active, remote-deleted, refining, owes a Drive replacement, has an owed
// Drive folder, or has a folder whose documents still need refresh. It returns
// the deleted session IDs so GC can erase each now-redundant raw capture.
func (s *SQLiteStore) DeleteEndedSessionAndSources(id, audioPath, audioDir string) ([]string, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin deleting session %s and its sources: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var block GCSessionBlockError
	err = tx.QueryRow(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) SELECT member.id, CASE
			WHEN member.status = ? THEN 'active'
			WHEN member.sync_state = ? THEN 'remote-deleted'
			WHEN member.refinement_status = ? THEN 'refinement running'
			WHEN member.drive_replace_pending = 1 THEN 'Drive replacement owed'
			WHEN EXISTS (
				SELECT 1 FROM drive_replacement_folders
				WHERE drive_replacement_folders.folder_session_id = member.id
				  AND drive_replacement_folders.state = ?
			) THEN 'Drive folder replacement owed'
			ELSE 'documents need Drive refresh'
		END
		FROM sessions AS member JOIN doomed ON member.id = doomed.id
		WHERE (
			member.status = ?
			OR member.sync_state = ?
			OR member.refinement_status = ?
			OR member.drive_replace_pending = 1
			OR EXISTS (
				SELECT 1 FROM drive_replacement_folders
				WHERE drive_replacement_folders.folder_session_id = member.id
				  AND drive_replacement_folders.state = ?
			)
			OR (member.gdrive_folder_id != '' AND member.drive_documents_pending != 0)
		  )
		LIMIT 1`,
		id, SessionActive, SyncStateRemoteDeleted, RefinementRunning, DriveReplacementFolderOwed,
		SessionActive, SyncStateRemoteDeleted, RefinementRunning, DriveReplacementFolderOwed).Scan(&block.BlockingSessionID, &block.Reason)
	switch {
	case err == nil:
		block.BlockingSessionIsTarget = block.BlockingSessionID == id
		return nil, false, &block
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, fmt.Errorf("check session set of %s before GC deletion: %w", id, err)
	}
	var eligible int
	err = tx.QueryRow(`SELECT 1 FROM sessions
		WHERE sessions.id = ? AND sessions.audio_path = ? AND sessions.documents_unverified = 0
		  AND `+removableCondition,
		id, audioPath).Scan(&eligible)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("check session %s and its sources before GC deletion: %w", id, err)
	}
	rows, err := tx.Query(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) SELECT id FROM doomed`, id)
	if err != nil {
		return nil, false, fmt.Errorf("list source sessions of %s for GC deletion: %w", id, err)
	}
	var deletedIDs []string
	for rows.Next() {
		var deletedID string
		if err := rows.Scan(&deletedID); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("scan source session of %s for GC deletion: %w", id, err)
		}
		deletedIDs = append(deletedIDs, deletedID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, fmt.Errorf("list source sessions of %s for GC deletion: %w", id, err)
	}
	if audioDir != "" {
		for _, deletedID := range deletedIDs {
			if err := recordEraseDirectory(tx, filepath.Dir(recording.RawPath(audioDir, deletedID))); err != nil {
				return nil, false, fmt.Errorf("record raw erase directory for session %s: %w", deletedID, err)
			}
		}
	}
	if err := dropSessionSetEvents(tx, id); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) DELETE FROM sessions WHERE id IN (SELECT id FROM doomed)`, id); err != nil {
		return nil, false, fmt.Errorf("delete session %s and its sources for GC: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit GC deletion of session %s and its sources: %w", id, err)
	}
	return deletedIDs, true, nil
}
