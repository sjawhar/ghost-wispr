package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	DriveReplacementUploading = "uploading"
	DriveReplacementUploaded  = "uploaded"
	DriveReplacementDeleted   = "deleted"
)

// The states of a folder in the ledger of a segment deletion
// (drive_replacement_folders).
const (
	// DriveReplacementFolderOwed: the folder may still hold the deleted words.
	DriveReplacementFolderOwed = "owed"
	// DriveReplacementFolderReplaced: its old files were replaced and deleted.
	DriveReplacementFolderReplaced = "replaced"
	// DriveReplacementFolderGone: Drive answered that the folder is gone for
	// good, its old files with it.
	DriveReplacementFolderGone = "gone"
)

// DriveReplacementFolder is one Drive folder a segment deletion owes a
// replacement: that of session FolderSessionID, in state State.
type DriveReplacementFolder struct {
	FolderSessionID string
	State           string
}

// recordOwedDriveReplacementFolders records, in tx, that sessionID's segment
// deletion at its current deletion generation owes a replacement to the Drive
// folder of each of folderSessionIDs that has one. The ledger rows of the
// session's earlier deletions go: the new generation covers every folder.
func recordOwedDriveReplacementFolders(tx *sql.Tx, sessionID string, folderSessionIDs []string) error {
	if _, err := tx.Exec(`DELETE FROM drive_replacement_folders WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("clear earlier owed Drive folders of session %s: %w", sessionID, err)
	}
	for _, folderSessionID := range folderSessionIDs {
		if _, err := tx.Exec(`INSERT INTO drive_replacement_folders(session_id, generation, folder_session_id, state)
			SELECT ?, (SELECT deletion_generation FROM sessions WHERE id = ?), id, ?
			FROM sessions WHERE id = ? AND gdrive_folder_id != ''`,
			sessionID, sessionID, DriveReplacementFolderOwed, folderSessionID); err != nil {
			return fmt.Errorf("record owed Drive folder of session %s for session %s: %w", folderSessionID, sessionID, err)
		}
	}
	return nil
}

// DriveReplacementFolders returns the ledger of the Drive folders sessionID's
// segment deletion at generation owes a replacement.
func (s *SQLiteStore) DriveReplacementFolders(sessionID string, generation int) ([]DriveReplacementFolder, error) {
	rows, err := s.db.Query(`SELECT folder_session_id, state FROM drive_replacement_folders
		WHERE session_id = ? AND generation = ? ORDER BY folder_session_id`, sessionID, generation)
	if err != nil {
		return nil, fmt.Errorf("list owed Drive folders of session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()
	var folders []DriveReplacementFolder
	for rows.Next() {
		var folder DriveReplacementFolder
		if err := rows.Scan(&folder.FolderSessionID, &folder.State); err != nil {
			return nil, fmt.Errorf("scan owed Drive folder of session %s: %w", sessionID, err)
		}
		folders = append(folders, folder)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate owed Drive folders of session %s: %w", sessionID, err)
	}
	return folders, nil
}

// EnsureDriveReplacementFolders returns the ledger of the Drive folders
// sessionID's refined audio deletion at generation owes, rebuilding it first
// while that deletion is still pending: from the session's own folder and
// each merged source's that has one.
// A pending deletion with no ledger was made by a build that has none (one
// deploy/pi/update.sh rolled back to, after this version's migration had
// run), and read as owing nothing it would leave the words in those folders.
// The ledger it returns is empty only if no such folder exists.
func (s *SQLiteStore) EnsureDriveReplacementFolders(sessionID string, generation int) ([]DriveReplacementFolder, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin checking the owed Drive folders of session %s: %w", sessionID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var recorded int
	if err := tx.QueryRow(`SELECT count(*) FROM drive_replacement_folders WHERE session_id = ? AND generation = ?`, sessionID, generation).Scan(&recorded); err != nil {
		return nil, fmt.Errorf("count owed Drive folders of session %s: %w", sessionID, err)
	}
	if recorded == 0 {
		var pending int
		err := tx.QueryRow(`SELECT 1 FROM sessions WHERE id = ? AND drive_replace_pending = 1 AND deletion_generation = ?`, sessionID, generation).Scan(&pending)
		switch {
		case err == nil:
			sources, err := mergedSourceIDs(tx, sessionID)
			if err != nil {
				return nil, err
			}
			if err := recordOwedDriveReplacementFolders(tx, sessionID, append([]string{sessionID}, sources...)); err != nil {
				return nil, err
			}
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("check the pending deletion of session %s: %w", sessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit the owed Drive folders of session %s: %w", sessionID, err)
	}
	return s.DriveReplacementFolders(sessionID, generation)
}

// MarkDriveReplacementFolder records that the folder of folderSessionID, owed
// by sessionID's deletion at generation, is replaced or gone. Replaced needs
// the folder's replacement recorded for generation, with every old file of it
// deleted: a replacement always uploads summary.md and transcript.md, each
// with a row (one with no old id when the folder had no such file), so a
// folder with no rows was never replaced, even one that held no files. It
// changes nothing, and errs, once a later deletion moved sessionID's
// generation, for a folder no longer owed, or for a replaced folder with no
// rows or an old file left.
func (s *SQLiteStore) MarkDriveReplacementFolder(sessionID string, generation int, folderSessionID, state string) error {
	switch state {
	case DriveReplacementFolderReplaced, DriveReplacementFolderGone:
	default:
		return fmt.Errorf("mark Drive folder of session %s: %q is not a finished folder state", folderSessionID, state)
	}
	result, err := s.db.Exec(`UPDATE drive_replacement_folders SET state = ?
		WHERE session_id = ? AND generation = ? AND folder_session_id = ? AND state = ?
		  AND EXISTS (SELECT 1 FROM sessions WHERE id = ? AND deletion_generation = ?)
		  AND (? = ? OR (
		       EXISTS (SELECT 1 FROM drive_replacements WHERE session_id = ? AND generation = ?)
		       AND NOT EXISTS (SELECT 1 FROM drive_replacements WHERE session_id = ? AND generation = ? AND state != ?)))`,
		state, sessionID, generation, folderSessionID, DriveReplacementFolderOwed,
		sessionID, generation,
		state, DriveReplacementFolderGone,
		folderSessionID, generation,
		folderSessionID, generation, DriveReplacementDeleted)
	if err != nil {
		return fmt.Errorf("mark Drive folder of session %s %s: %w", folderSessionID, state, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark Drive folder of session %s rows affected: %w", folderSessionID, err)
	}
	if rows == 0 {
		return fmt.Errorf("mark Drive folder of session %s %s for session %s generation %d: it is no longer owed, the generation moved, or an old file is left: %w",
			folderSessionID, state, sessionID, generation, sql.ErrNoRows)
	}
	return nil
}

// DriveReplacement records one old Drive file and the replacement uploaded for
// it. A duplicate filename can have several rows so every copy is removed.
type DriveReplacement struct {
	SessionID  string
	Generation int
	Name       string
	OldID      string
	NewID      string
	State      string
}

// DriveReplacementInput reports whether sessionID needs a Drive replacement
// and the most recently spliced recording for that replacement.
func (s *SQLiteStore) DriveReplacementInput(sessionID string) (bool, string, error) {
	var pending int
	if err := s.db.QueryRow(`SELECT drive_replace_pending FROM sessions WHERE id = ?`, sessionID).Scan(&pending); err != nil {
		return false, "", fmt.Errorf("read drive replacement for session %s: %w", sessionID, err)
	}
	if pending == 0 {
		return false, "", nil
	}

	var audioPath string
	if err := s.db.QueryRow(`SELECT audio_path FROM deleted_segments WHERE session_id = ? ORDER BY rowid DESC LIMIT 1`, sessionID).Scan(&audioPath); err != nil {
		if err == sql.ErrNoRows {
			return false, "", fmt.Errorf("read drive replacement for session %s: no deleted segment", sessionID)
		}
		return false, "", fmt.Errorf("read spliced recording for session %s: %w", sessionID, err)
	}
	return true, audioPath, nil
}

func (s *SQLiteStore) PendingDriveReplacementRecordings(sessionID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT audio_path FROM deleted_segments WHERE session_id = ? ORDER BY audio_path`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list spliced recordings for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var recordings []string
	for rows.Next() {
		var recording string
		if err := rows.Scan(&recording); err != nil {
			return nil, fmt.Errorf("scan spliced recording for session %s: %w", sessionID, err)
		}
		recordings = append(recordings, recording)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spliced recordings for session %s: %w", sessionID, err)
	}
	return recordings, nil
}

func (s *SQLiteStore) ClearDriveReplacementPending(sessionID string, generation int) error {
	result, err := s.db.Exec(`UPDATE sessions SET drive_replace_pending = 0, drive_replace_since = '', drive_replace_failures = 0, drive_replace_error = ''
		WHERE id = ? AND drive_replace_pending = 1 AND deletion_generation = ?`, sessionID, generation)
	if err != nil {
		return fmt.Errorf("clear drive replacement for session %s: %w", sessionID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count cleared drive replacement for session %s: %w", sessionID, err)
	}
	if rows == 0 {
		return fmt.Errorf("clear drive replacement for session %s: %w", sessionID, sql.ErrNoRows)
	}
	return nil
}

// settleUnownedDriveReplacement clears, in tx, session id's pending Drive
// replacement once its segment deletion owes Google Drive nothing, so that
// with sync off a session no Drive folder holds is not left owing one (merge
// and delete refused, a warning that Drive holds the words). That is so once
// every splice of the session is done (until then the mark keeps a first
// sync from uploading the unspliced recording), no sync holds the session (a
// sync under way may have made a folder it has not recorded yet: its end
// records it, oweFolderOfPendingDeletion, and settles again), the ledger
// owes no folder at the current generation, and neither the session nor any
// session merged into it, directly or through another merge, has a Drive
// folder. MarkSpliceComplete, EndSync and MarkSynced call it.
func settleUnownedDriveReplacement(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`UPDATE sessions SET drive_replace_pending = 0, drive_replace_since = '', drive_replace_failures = 0, drive_replace_error = ''
		WHERE id = ? AND drive_replace_pending = 1 AND sync_state != ?
		  AND NOT EXISTS (SELECT 1 FROM deleted_segments WHERE deleted_segments.session_id = sessions.id AND splice_complete = 0)
		  AND NOT EXISTS (SELECT 1 FROM drive_replacement_folders WHERE drive_replacement_folders.session_id = sessions.id AND generation = sessions.deletion_generation)
		  AND NOT EXISTS (
			WITH RECURSIVE held(id) AS (
				SELECT ?
				UNION SELECT merged.id FROM sessions AS merged JOIN held ON merged.merged_into = held.id
			) SELECT 1 FROM sessions AS folder JOIN held ON folder.id = held.id WHERE folder.gdrive_folder_id != '')`,
		id, SyncStateSyncing, id); err != nil {
		return fmt.Errorf("settle the Drive replacement of session %s: %w", id, err)
	}
	return nil
}

// OwedDriveReplacement is a session that still owes Google Drive a
// replacement after a segment deletion: since when (its first deletion not
// yet replaced), and how many replacement attempts failed in a row since.
type OwedDriveReplacement struct {
	SessionID string
	Since     time.Time
	Failures  int
	LastError string
}

// RecordDriveReplacementFailure counts a failed replacement attempt of
// sessionID's deletion generation, with why it failed. An attempt for an
// earlier generation, or a session that no longer owes one, records nothing.
func (s *SQLiteStore) RecordDriveReplacementFailure(sessionID string, generation int, message string) error {
	if _, err := s.db.Exec(`UPDATE sessions SET drive_replace_failures = drive_replace_failures + 1, drive_replace_error = ?
		WHERE id = ? AND drive_replace_pending = 1 AND deletion_generation = ?`, message, sessionID, generation); err != nil {
		return fmt.Errorf("record drive replacement failure for session %s: %w", sessionID, err)
	}
	return nil
}

// OwedDriveReplacements returns every session that still owes Drive a
// replacement, as the store holds it, so it survives a restart.
func (s *SQLiteStore) OwedDriveReplacements() ([]OwedDriveReplacement, error) {
	rows, err := s.db.Query(`SELECT id, drive_replace_since, drive_replace_failures, drive_replace_error
		FROM sessions WHERE drive_replace_pending = 1 ORDER BY started_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list owed drive replacements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var owed []OwedDriveReplacement
	for rows.Next() {
		var replacement OwedDriveReplacement
		var since string
		if err := rows.Scan(&replacement.SessionID, &since, &replacement.Failures, &replacement.LastError); err != nil {
			return nil, fmt.Errorf("scan owed drive replacement: %w", err)
		}
		if since != "" {
			if replacement.Since, err = time.Parse(time.RFC3339Nano, since); err != nil {
				return nil, fmt.Errorf("parse owed drive replacement time of session %s: %w", replacement.SessionID, err)
			}
		}
		owed = append(owed, replacement)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate owed drive replacements: %w", err)
	}
	return owed, nil
}
func (s *SQLiteStore) DriveReplacements(sessionID string, generation int) ([]DriveReplacement, error) {
	rows, err := s.db.Query(`SELECT session_id, generation, name, old_id, new_id, state
		FROM drive_replacements WHERE session_id = ? AND generation = ? ORDER BY name, old_id`, sessionID, generation)
	if err != nil {
		return nil, fmt.Errorf("list drive replacements for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var replacements []DriveReplacement
	for rows.Next() {
		var replacement DriveReplacement
		if err := rows.Scan(&replacement.SessionID, &replacement.Generation, &replacement.Name, &replacement.OldID, &replacement.NewID, &replacement.State); err != nil {
			return nil, fmt.Errorf("scan drive replacement for session %s: %w", sessionID, err)
		}
		replacements = append(replacements, replacement)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate drive replacements for session %s: %w", sessionID, err)
	}
	return replacements, nil
}

// StartDriveReplacement writes every old id before uploading its replacement.
// An empty old id represents a missing Drive file that must be uploaded but
// has nothing to delete.
func (s *SQLiteStore) StartDriveReplacement(sessionID, name string, generation int, oldIDs []string) error {
	if sessionID == "" || name == "" {
		return fmt.Errorf("start drive replacement: session and name are required")
	}
	if len(oldIDs) == 0 {
		oldIDs = []string{""}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin drive replacement %s/%s: %w", sessionID, name, err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing int
	if err := tx.QueryRow(`SELECT count(*) FROM drive_replacements WHERE session_id = ? AND generation = ? AND name = ?`, sessionID, generation, name).Scan(&existing); err != nil {
		return fmt.Errorf("check drive replacement %s/%s: %w", sessionID, name, err)
	}
	if existing != 0 {
		return fmt.Errorf("start drive replacement %s/%s: rows already exist", sessionID, name)
	}
	seen := make(map[string]bool, len(oldIDs))
	for _, oldID := range oldIDs {
		if seen[oldID] {
			return fmt.Errorf("start drive replacement %s/%s: old ids must be unique", sessionID, name)
		}
		seen[oldID] = true
		if _, err := tx.Exec(`INSERT INTO drive_replacements(session_id, generation, name, old_id, new_id, state) VALUES(?, ?, ?, ?, '', ?)`,
			sessionID, generation, name, oldID, DriveReplacementUploading); err != nil {
			return fmt.Errorf("record drive replacement %s/%s: %w", sessionID, name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit drive replacement %s/%s: %w", sessionID, name, err)
	}
	return nil
}

func (s *SQLiteStore) MarkDriveReplacementUploaded(sessionID, name string, generation int, newID string) error {
	if newID == "" {
		return fmt.Errorf("mark drive replacement uploaded %s/%s: new id is required", sessionID, name)
	}
	result, err := s.db.Exec(`UPDATE drive_replacements SET new_id = ?, state = ?
		WHERE session_id = ? AND name = ? AND generation = ? AND state = ?`,
		newID, DriveReplacementUploaded, sessionID, name, generation, DriveReplacementUploading)
	if err != nil {
		return fmt.Errorf("mark drive replacement uploaded %s/%s: %w", sessionID, name, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count drive replacement upload %s/%s: %w", sessionID, name, err)
	}
	if rows == 0 {
		return fmt.Errorf("mark drive replacement uploaded %s/%s: %w", sessionID, name, sql.ErrNoRows)
	}
	return nil
}

func (s *SQLiteStore) MarkDriveReplacementDeleted(sessionID, name, oldID string, generation int) error {
	result, err := s.db.Exec(`UPDATE drive_replacements SET state = ?
		WHERE session_id = ? AND name = ? AND old_id = ? AND generation = ? AND state = ?`,
		DriveReplacementDeleted, sessionID, name, oldID, generation, DriveReplacementUploaded)
	if err != nil {
		return fmt.Errorf("mark drive replacement deleted %s/%s: %w", sessionID, name, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count drive replacement deletion %s/%s: %w", sessionID, name, err)
	}
	if rows == 0 {
		return fmt.Errorf("mark drive replacement deleted %s/%s: %w", sessionID, name, sql.ErrNoRows)
	}
	return nil
}

// FinishDriveReplacement clears sessionID's pending marker once its segment
// deletion at generation owes no Drive folder any more: the ledger has a
// folder for it, and every one is replaced or gone. It refuses, changing
// nothing, while a folder is owed, for a generation with no ledger, and once
// a later deletion moved the session's generation.
func (s *SQLiteStore) FinishDriveReplacement(sessionID string, generation int) error {
	result, err := s.db.Exec(`UPDATE sessions SET drive_replace_pending = 0, drive_replace_since = '', drive_replace_failures = 0, drive_replace_error = ''
		WHERE id = ? AND drive_replace_pending = 1 AND deletion_generation = ?
		  AND EXISTS (SELECT 1 FROM drive_replacement_folders WHERE session_id = ? AND generation = ?)
		  AND NOT EXISTS (SELECT 1 FROM drive_replacement_folders WHERE session_id = ? AND generation = ? AND state = ?)`,
		sessionID, generation, sessionID, generation, sessionID, generation, DriveReplacementFolderOwed)
	if err != nil {
		return fmt.Errorf("finish drive replacement for session %s: %w", sessionID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count completed drive replacement for session %s: %w", sessionID, err)
	}
	if rows != 0 {
		return nil
	}
	var owed int
	if err := s.db.QueryRow(`SELECT count(*) FROM drive_replacement_folders WHERE session_id = ? AND generation = ? AND state = ?`,
		sessionID, generation, DriveReplacementFolderOwed).Scan(&owed); err != nil {
		return fmt.Errorf("check owed Drive folders of session %s: %w", sessionID, err)
	}
	if owed != 0 {
		return fmt.Errorf("finish drive replacement for session %s: %d Drive folders still owed", sessionID, owed)
	}
	return fmt.Errorf("finish drive replacement for session %s: %w", sessionID, sql.ErrNoRows)
}
