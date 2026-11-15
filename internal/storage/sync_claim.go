package storage

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// ClaimSync claims session id for one sync, which claim names: it moves the
// session to SYNCING only while its sync state is still from (the state the
// sync read) and no sync holds it (from is not SYNCING), and returns the
// session as claimed. So two syncs of one session (retry-sync, the sync after
// a summary, the sweep) never run at once: the second finds it claimed and
// starts nothing. The session stays the sync's until MarkSynced or EndSync,
// under the same claim, ends it, or the app restarts
// (RecoverInterruptedSyncs), which reads from (sync_claimed_from).
func (s *SQLiteStore) ClaimSync(id, from, claim string, at time.Time) (Session, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, false, fmt.Errorf("begin claiming the sync of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`UPDATE sessions SET sync_state = ?, sync_status = ?, sync_claim = ?, sync_claimed_from = ?, last_sync_attempt = ?, error_message = ''
		 WHERE id = ? AND sync_state = ? AND sync_state != ?`,
		SyncStateSyncing, syncStatusFromState(SyncStateSyncing), claim, from, at.UTC().Format(time.RFC3339Nano),
		id, from, SyncStateSyncing,
	)
	if err != nil {
		return Session{}, false, fmt.Errorf("claim the sync of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return Session{}, false, fmt.Errorf("claim sync rows affected: %w", err)
	}
	if rows == 0 {
		return Session{}, false, nil
	}
	sess, err := getSession(tx, id)
	if err != nil {
		return Session{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, false, fmt.Errorf("commit claiming the sync of session %s: %w", id, err)
	}
	return sess, true, nil
}

// MarkSynced marks session id synced to Drive folder driveFolderID, only while
// the sync claim names still holds it (ClaimSync) and unchanged reports true
// for the session and its segments as they are now, read in the same
// transaction as the update, and reports whether it did. A sync passes a
// check that the session is still what it uploaded: its audio path (GC may
// have put back a recording it could not remove) and its documents (a
// summary made again, a refined transcript). A session that changed during
// the upload is not synced, and the next sync uploads what it is now; one
// the sync no longer holds is left as it is. Marked synced, Drive has the
// session's documents as they are, so they are verified (documents_unverified
// is cleared). unchanged runs inside the transaction: it must not use the
// store.
func (s *SQLiteStore) MarkSynced(id, claim, driveFolderID string, unchanged func(Session, []transcribe.Segment) bool) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin marking session %s synced: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	sess, err := getSession(tx, id)
	if err != nil {
		return false, err
	}
	segments, err := getSegments(tx, id)
	if err != nil {
		return false, err
	}
	if !unchanged(sess, transcriptSegments(segments)) {
		return false, nil
	}
	res, err := tx.Exec(
		`UPDATE sessions
		 SET sync_state = ?, sync_status = ?, gdrive_folder_id = CASE WHEN ? != '' THEN ? ELSE gdrive_folder_id END,
		     retry_count = 0, last_sync_attempt = NULL, error_message = '', sync_claim = '', sync_claimed_from = '', documents_unverified = 0, drive_documents_pending = 0
		 WHERE id = ? AND sync_state = ? AND sync_claim = ?`,
		SyncStateSynced, SyncSynced, driveFolderID, driveFolderID,
		id, SyncStateSyncing, claim,
	)
	if err != nil {
		return false, fmt.Errorf("mark session %s synced: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark synced rows affected: %w", err)
	}
	if rows == 0 {
		return false, nil
	}
	if err := oweFolderOfPendingDeletion(tx, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit marking session %s synced: %w", id, err)
	}
	return true, nil
}

// RecordSyncFolder records a folder just created for the sync claim that
// names it, before that sync uploads its first file. A stopped process then
// leaves a retry with the folder to reuse. When a segment deletion is pending,
// the same transaction puts the folder in its replacement ledger.
func (s *SQLiteStore) RecordSyncFolder(id, claim, driveFolderID string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin recording Drive folder of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE sessions SET gdrive_folder_id = ?
		WHERE id = ? AND sync_state = ? AND sync_claim = ?`,
		driveFolderID, id, SyncStateSyncing, claim)
	if err != nil {
		return false, fmt.Errorf("record Drive folder of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count recorded Drive folder of session %s: %w", id, err)
	}
	if rows == 0 {
		return false, nil
	}
	if err := oweFolderOfPendingDeletion(tx, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit Drive folder of session %s: %w", id, err)
	}
	return true, nil
}

// EndSync records the end of the sync claim names, which did not mark
// session id synced: its sync state becomes state (back to pending when the
// session changed during the upload, a retry, a failure, or its Drive folder
// gone), with its retry count, the time it was tried and why it did not
// finish, and the Drive folder the sync made, if it made one (one the session
// had stays). It does this only while that sync still holds the session, so
// a sync that no longer does never replaces what the session is now, and
// reports whether it did. Any other state is an error and changes nothing:
// SYNCED would skip MarkSynced's check that Drive has the session as it is
// now, and SYNCING would leave the session held by no sync. A sync that held
// the session while a refined segment was deleted uploaded the words before
// the deletion; the folder it made is recorded as owed to that deletion's
// replacement in the same transaction.
func (s *SQLiteStore) EndSync(id, claim, state, driveFolderID string, retryCount int, at time.Time, errorMessage string) (bool, error) {
	switch state {
	case SyncStatePending, SyncStateRetryScheduled, SyncStateFailed, SyncStateRemoteDeleted:
	default:
		return false, fmt.Errorf("end the sync of session %s: %q is not a state a sync ends in unsynced", id, state)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin ending the sync of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`UPDATE sessions
		 SET sync_state = ?, sync_status = ?, gdrive_folder_id = CASE WHEN ? != '' THEN ? ELSE gdrive_folder_id END,
		     retry_count = ?, last_sync_attempt = ?, error_message = ?, sync_claim = '', sync_claimed_from = ''
		 WHERE id = ? AND sync_state = ? AND sync_claim = ?`,
		state, syncStatusFromState(state), driveFolderID, driveFolderID,
		retryCount, at.UTC().Format(time.RFC3339Nano), errorMessage,
		id, SyncStateSyncing, claim,
	)
	if err != nil {
		return false, fmt.Errorf("end the sync of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("end sync rows affected: %w", err)
	}
	if rows != 1 {
		return false, nil
	}
	if err := oweFolderOfPendingDeletion(tx, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit the end of the sync of session %s: %w", id, err)
	}
	return true, nil
}

// oweFolderOfPendingDeletion records, in tx, the Drive folder a sync just
// recorded for session id as owed to every pending refined deletion that
// contains that session, directly or through a merge. A sync that held either
// the owner or a merged source while a segment was deleted uploaded the words
// before the deletion. A sync that made no folder leaves nothing to owe, and
// its deletion is settled only once no folder can hold the words.
func oweFolderOfPendingDeletion(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`WITH RECURSIVE owners(id) AS (
			SELECT ?
			UNION SELECT source.merged_into FROM sessions AS source JOIN owners ON source.id = owners.id WHERE source.merged_into != ''
		) INSERT OR IGNORE INTO drive_replacement_folders(session_id, generation, folder_session_id, state)
		SELECT owner.id, owner.deletion_generation, ?, ?
		FROM sessions AS owner JOIN owners ON owner.id = owners.id
		WHERE owner.drive_replace_pending = 1
		  AND EXISTS (SELECT 1 FROM sessions AS folder WHERE folder.id = ? AND folder.gdrive_folder_id != '')`,
		id, id, DriveReplacementFolderOwed, id); err != nil {
		return fmt.Errorf("owe the Drive folder of session %s its replacement: %w", id, err)
	}
	return settleUnownedDriveReplacement(tx, id)
}

// RecoverInterruptedSyncs moves every session an earlier run of the app left
// SYNCING (it stopped during the sync, so no sync holds the session now) back
// to a retry, keeping its Drive folder, and returns their ids. The app calls
// it as it starts, before any sync. The cut-off sync counts as a failed
// attempt, as in gdrive.BuildRetryPlan: the session is RETRY_SCHEDULED with
// one retry more, or FAILED once it has had maxRetries, so a sync that stops
// the app every time it runs is not retried for ever. A sync claimed from
// REMOTE_DELETED (a retry that found the session's Drive folder back) goes
// back to REMOTE_DELETED, its retry count as it was: Drive may have lost the
// folder again since, and only the next retry, asking Drive again, takes the
// session out of it. A session that still owes Drive a replacement
// (drive_replace_pending) goes back to the state its claim came from, its
// retry count as it was: a deletion is never left FAILED, where only a manual
// retry would remove the deleted words from Drive.
func (s *SQLiteStore) RecoverInterruptedSyncs(maxRetries int, at time.Time) ([]string, error) {
	// The state a cut-off replacement goes back to: the one its claim came
	// from, PENDING when that was SYNCED or is not recorded.
	const replacementBack = `CASE WHEN sync_claimed_from IN ('', ?) THEN ? ELSE sync_claimed_from END`
	rows, err := s.db.Query(
		`UPDATE sessions
		 SET sync_state = CASE WHEN drive_replace_pending = 1 THEN `+replacementBack+`
		                       WHEN sync_claimed_from = ? THEN ? WHEN retry_count >= ? THEN ? ELSE ? END,
		     sync_status = CASE WHEN drive_replace_pending = 1 AND `+replacementBack+` = ? THEN ? ELSE ? END,
		     retry_count = CASE WHEN drive_replace_pending = 1 OR sync_claimed_from = ? OR retry_count >= ? THEN retry_count ELSE retry_count + 1 END,
		     last_sync_attempt = ?,
		     error_message = CASE WHEN drive_replace_pending = 1 THEN ? WHEN sync_claimed_from = ? THEN ? ELSE ? END,
		     sync_claim = '', sync_claimed_from = ''
		 WHERE sync_state = ?
		 RETURNING id`,
		SyncStateSynced, SyncStatePending,
		SyncStateRemoteDeleted, SyncStateRemoteDeleted, maxRetries, SyncStateFailed, SyncStateRetryScheduled,
		SyncStateSynced, SyncStatePending, SyncStatePending, SyncPending, SyncFailed,
		SyncStateRemoteDeleted, maxRetries,
		at.UTC().Format(time.RFC3339Nano),
		"a Drive replacement after a segment deletion was cut off: the app stopped during it",
		SyncStateRemoteDeleted, "a retry after the Drive folder was deleted was cut off: the app stopped during it", "the sync was cut off: the app stopped during it",
		SyncStateSyncing,
	)
	if err != nil {
		return nil, fmt.Errorf("recover interrupted syncs: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan recovered sync: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("recover interrupted syncs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close recovered syncs: %w", err)
	}
	return ids, nil
}
