package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/recording"
)

// sessionsDeletedTrigger names the trigger that records, in app_state
// (sessionsDeletedKey and sessionsDeletedUnsweptKey), that a session row was
// deleted. It lives in the database, so it records a delete by any build, an
// older one after an update rolled back included.
const sessionsDeletedTrigger = "sessions_deleted_mark"

// sessionsDeletedKey is the app_state key the build before this one
// (a867073) reads and clears: a session row was deleted, and what it left may
// not be cleared yet. sessionsDeletedTrigger still writes it, for that build
// after a rollback.
const sessionsDeletedKey = "sessions_deleted_unswept"

// sessionsDeletedUnsweptKey is the app_state key sessionsDeletedTrigger
// writes with sessionsDeletedKey, and that only this build clears: a session
// row was deleted, and what it left (its events, the status events an older
// build stored with its id only in their message, its refused cuts and
// summary requests, its words in the search index) may not be cleared yet.
// The build before this one cleared sessionsDeletedKey in its delete's
// transaction after deleting only the events that name the session in
// session_id, so that key alone does not say its delete left nothing. A
// delete by this build clears both in its own transaction
// (clearSessionsDeleted); the next start clears what any other delete left
// (dropWhatDeletedSessionsLeft).
const sessionsDeletedUnsweptKey = "sessions_deleted_unswept_v2"

// DeleteSession deletes session id and its merged-source set, refusing a target
// that feeds another merge (ErrDeleteMergeActive), any active member, or any
// member that owes Google Drive a replacement
// (ErrDeleteReplacementPending). It returns the audio paths the deleted rows
// name: the recordings the caller then erases are the ones the rows named
// when they went, not ones it read before. In the same transaction, before
// the rows go, it records the directory of each of those recordings, found
// where recording.Resolve finds it in audioDir (the one the caller then
// erases), so an erase a stop cuts off is finished as the app starts, and it
// records each session it deletes with every audio file of it in audioDir
// (deleteFiles: the recordings its row names and its restored Drive folder
// holds, its raw capture, <id>.mp3, <id>.wav and a save's temporary files),
// so the caller and, after a stop, the kept recordings pass erase them; it
// deletes nothing if it cannot record one. In that transaction too it
// deletes every stored
// event that names a session it deletes: a live transcript event holds the
// conversation's words, a summary event its summary, and a component status
// event that it existed and was refined and synced. It deletes the events
// that name one in session_id before the rows go (dropSessionSetEvents), and
// those an older build stored with the session only in a status message, and
// the refused cuts and summary requests that name one, once they have gone
// (clearSessionsDeleted). Once the delete commits, it erases it from the
// write-ahead log and the pre-migration backup, as a segment deletion does
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
	if err := recordDeletedSessionAudio(tx, deletedIDs, func(sessionID, sessionAudioPath string) ([]string, error) {
		return deleteFiles(tx, sessionID, sessionAudioPath, audioDir)
	}); err != nil {
		return "", fmt.Errorf("delete session %s: %w", id, err)
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
	if err := rebuildSearchIndex(tx); err != nil {
		return "", fmt.Errorf("delete session %s: %w", id, err)
	}
	if _, err := clearSessionsDeleted(tx); err != nil {
		return "", fmt.Errorf("delete session %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit deleting session %s: %w", id, err)
	}
	s.eraseAfterSessionDeletion(owed, earlierOwed, deletedIDs)
	return audioPath, nil
}

// rebuildSearchIndex rebuilds the search index (sessions_fts) in tx from the
// rows that are left. A row's delete only marks its terms deleted in the
// index, which keeps the old entries, and so the row's words, in the file
// (FTS5's secure-delete is off); a delete that takes a conversation's words
// off the unit rebuilds the index, and the pages that frees are zeroed
// (secure_delete), as a segment deletion does.
func rebuildSearchIndex(tx *sql.Tx) error {
	if _, err := tx.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild the search index: %w", err)
	}
	return nil
}

// clearSessionsDeleted deletes, in tx, the stored events of every session the
// database no longer has (dropEventsOfDeletedSessions) and the other rows
// that name one (dropRowsOfDeletedSessions), and records that nothing a
// deleted session left remains (sessionsDeletedKey and
// sessionsDeletedUnsweptKey); tx has rebuilt the search index. It sweeps
// every deleted session, not only the ones tx deleted: the record may stand
// for a delete made outside the app (with sqlite3), which nothing else would
// sweep once the record is gone. It returns how many events it deleted.
func clearSessionsDeleted(tx *sql.Tx) (int64, error) {
	removed, err := dropEventsOfDeletedSessions(tx)
	if err != nil {
		return 0, err
	}
	if err := dropRowsOfDeletedSessions(tx); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM app_state WHERE key IN (?, ?)`, sessionsDeletedKey, sessionsDeletedUnsweptKey); err != nil {
		return 0, fmt.Errorf("record that the deleted sessions left nothing: %w", err)
	}
	return removed, nil
}

// dropRowsOfDeletedSessions deletes, in tx, the rows besides stored events
// that name a session the database does not have: a cut "Delete text and
// audio" refused (refused_cuts), which the status page forgets once its
// session is gone anyway, and a claimed summary request (summary_requests),
// which would also keep a session made later with the same id from claiming
// one, which no foreign key deletes with it; and the voice's questions
// (voice_questions) and questions to agents (voice_tasks), which hold the
// room's words and an agent's answer, and which their foreign key deletes
// only on a connection with foreign keys on, as the sqlite3 shell's is not.
// A table the database does not have yet is left alone.
//
// A deleted text span (deleted_text_spans) stays. It holds no words: the
// recording's file name, the span's times and the recording's SHA-256. It is
// what keeps the deleted words out if the recording comes back: a delete on
// the unit keeps the session's Drive copy, so a later restore from Drive can
// bring the recording back, and the unit cannot know that a recording is gone
// for good. It belongs to its recording, not to the session the text was
// deleted in, and is read by the recording's name (TextDeletionSpans).
func dropRowsOfDeletedSessions(tx *sql.Tx) error {
	for _, table := range []string{"refused_cuts", "summary_requests", "voice_questions", "voice_tasks"} {
		if exists, err := tableExists(tx, table); err != nil {
			return fmt.Errorf("check table %s before deleting the rows of deleted sessions: %w", table, err)
		} else if !exists {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM ` + table + ` WHERE NOT EXISTS (SELECT 1 FROM sessions WHERE sessions.id = ` + table + `.session_id)`); err != nil {
			return fmt.Errorf("delete the %s rows of deleted sessions: %w", table, err)
		}
	}
	return nil
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
// session the database does not have, and returns how many it deleted: an
// event that names it in session_id, and a component_status event an older
// build stored with the session only in its message
// (dropLegacyStatusEventsOfDeletedSessions). Events that name no session (a
// pause, the microphone's status) stay.
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
	legacy, err := dropLegacyStatusEventsOfDeletedSessions(tx)
	if err != nil {
		return 0, err
	}
	return removed + legacy, nil
}

// dropWhatDeletedSessionsLeft takes off the unit what a delete that did not
// remove a session's events and index entries left: a delete by a build
// before this one, or by the old build after an update rolled back
// (deploy/pi/update.sh) once this one had started. It deletes the stored
// events that name a session the database does not have
// (dropEventsOfDeletedSessions), which /api/events would serve for seven
// days, and the other rows that name one (dropRowsOfDeletedSessions), and
// rebuilds the search index (rebuildSearchIndex), which keeps a
// deleted row's words. The app runs it at every start, before it takes the
// pre-migration backup, so the backup holds none of them, and records the
// erase from the write-ahead log owed (markEraseOwed), which the start then
// finishes (retryOwedErasure). It runs only when a session row was deleted
// since (sessionsDeletedUnsweptKey), or when the database has no
// sessionsDeletedTrigger that records it yet (none, or the one the build
// before made, which records only sessionsDeletedKey) and so no record of
// what was deleted on it: a delete by the build before left status events
// with the session only in their message and cleared sessionsDeletedKey.
// Otherwise it writes nothing, and a start costs what it did before. A
// database that does not yet have every table this needs is left as it is,
// so nothing is created before the backup is taken.
func (s *SQLiteStore) dropWhatDeletedSessionsLeft() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin deleting what deleted sessions left: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"mcp_events", "sessions", "app_state"} {
		if exists, err := tableExists(tx, table); err != nil {
			return fmt.Errorf("check table %s before deleting what deleted sessions left: %w", table, err)
		} else if !exists {
			return nil
		}
	}
	var recorded, deleted int
	if err := tx.QueryRow(`SELECT
			(SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = ? AND instr(sql, ?) > 0),
			(SELECT count(*) FROM app_state WHERE key = ?)`,
		sessionsDeletedTrigger, sessionsDeletedUnsweptKey, sessionsDeletedUnsweptKey).Scan(&recorded, &deleted); err != nil {
		return fmt.Errorf("read whether a session was deleted since the last start: %w", err)
	}
	if recorded > 0 && deleted == 0 {
		return nil
	}
	if exists, err := tableExists(tx, "sessions_fts"); err != nil {
		return fmt.Errorf("check the search index before rebuilding it: %w", err)
	} else if exists {
		if err := rebuildSearchIndex(tx); err != nil {
			return err
		}
	}
	if err := markEraseOwed(tx); err != nil {
		return fmt.Errorf("delete what deleted sessions left: %w", err)
	}
	removed, err := clearSessionsDeleted(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit deleting what deleted sessions left: %w", err)
	}
	slog.Info("deleted what deleted sessions left and rebuilt the search index", "events", removed)
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
// them (dropSessionSetEvents), and rebuilds the search index so their words
// leave it (rebuildSearchIndex). It records every raw capture directory
// before the rows go, and each session it deletes for the kept recordings
// pass (recordDeletedSessionAudio). It refuses when a session in that
// deletion set is active, remote-deleted, refining, owes a Drive
// replacement, has an owed Drive folder, or has a folder whose documents
// still need refresh. Once the
// delete commits, it is erased from the write-ahead log and the pre-migration
// backup, as a person's delete is (eraseAfterSessionDeletion). It returns
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
	if audioDir != "" {
		if only, err := keepsOnlyRawAudio(id, audioPath, audioDir); err != nil {
			return nil, false, err
		} else if only {
			return nil, false, &GCSessionBlockError{BlockingSessionID: id, BlockingSessionIsTarget: true, Reason: "its only audio is its raw capture"}
		}
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
	if err := recordDeletedSessionAudio(tx, deletedIDs, func(sessionID, sessionAudioPath string) ([]string, error) {
		return ageDeleteFiles(sessionID, sessionAudioPath, audioDir), nil
	}); err != nil {
		return nil, false, fmt.Errorf("GC deletion of session %s: %w", id, err)
	}
	if err := dropSessionSetEvents(tx, id); err != nil {
		return nil, false, err
	}
	owed, earlierOwed, err := recordOwedErase(tx)
	if err != nil {
		return nil, false, fmt.Errorf("GC deletion of session %s: %w", id, err)
	}
	if _, err := tx.Exec(`WITH RECURSIVE doomed(id) AS (
			SELECT ?
			UNION SELECT source.id FROM sessions AS source JOIN doomed ON source.merged_into = doomed.id
		) DELETE FROM sessions WHERE id IN (SELECT id FROM doomed)`, id); err != nil {
		return nil, false, fmt.Errorf("delete session %s and its sources for GC: %w", id, err)
	}
	if err := rebuildSearchIndex(tx); err != nil {
		return nil, false, fmt.Errorf("GC deletion of session %s: %w", id, err)
	}
	if _, err := clearSessionsDeleted(tx); err != nil {
		return nil, false, fmt.Errorf("GC deletion of session %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit GC deletion of session %s and its sources: %w", id, err)
	}
	s.eraseAfterSessionDeletion(owed, earlierOwed, deletedIDs)
	return deletedIDs, true, nil
}

// keepsOnlyRawAudio reports whether session id's raw capture in audioDir is
// its only audio: the capture is there and no file its audio path names (found
// where recording.Resolve finds it) holds any audio. GC's age delete keeps
// such a session: deleting it would record the delete, and the kept
// recordings pass erases the audio of a recorded delete, while the row lets
// that pass save the capture.
func keepsOnlyRawAudio(id, audioPath, audioDir string) (bool, error) {
	info, err := os.Stat(recording.RawPath(audioDir, id))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check the raw capture of session %s: %w", id, err)
	case !info.Mode().IsRegular():
		return false, nil
	}
	for _, entry := range recording.Entries(audioPath) {
		if saved, err := os.Stat(recording.Resolve(entry, audioDir)); err == nil && saved.Mode().IsRegular() && saved.Size() > 0 {
			return false, nil
		}
	}
	return true, nil
}
