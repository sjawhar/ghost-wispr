package storage

import (
	"database/sql"
	"fmt"
)

const (
	DriveReplacementUploading = "uploading"
	DriveReplacementUploaded  = "uploaded"
	DriveReplacementDeleted   = "deleted"
)

// DriveReplacement records one old Drive file and the replacement uploaded for
// it. A duplicate filename can have several rows so every copy is removed.
type DriveReplacement struct {
	SessionID string
	Name      string
	OldID     string
	NewID     string
	State     string
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
	if err := s.db.QueryRow(`SELECT audio_path FROM deleted_segments WHERE session_id = ? ORDER BY deleted_at DESC, rowid DESC LIMIT 1`, sessionID).Scan(&audioPath); err != nil {
		if err == sql.ErrNoRows {
			return false, "", fmt.Errorf("read drive replacement for session %s: no deleted segment", sessionID)
		}
		return false, "", fmt.Errorf("read spliced recording for session %s: %w", sessionID, err)
	}
	return true, audioPath, nil
}

func (s *SQLiteStore) DriveReplacements(sessionID string) ([]DriveReplacement, error) {
	rows, err := s.db.Query(`SELECT session_id, name, old_id, new_id, state FROM drive_replacements WHERE session_id = ? ORDER BY name, old_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list drive replacements for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var replacements []DriveReplacement
	for rows.Next() {
		var replacement DriveReplacement
		if err := rows.Scan(&replacement.SessionID, &replacement.Name, &replacement.OldID, &replacement.NewID, &replacement.State); err != nil {
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
// The one transaction lets a retry recover the new file after a stop between
// Drive's upload response and the later uploaded-state write.
func (s *SQLiteStore) StartDriveReplacement(sessionID, name string, oldIDs []string) error {
	if sessionID == "" || name == "" || len(oldIDs) == 0 {
		return fmt.Errorf("start drive replacement: session, name and old ids are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin drive replacement %s/%s: %w", sessionID, name, err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing int
	if err := tx.QueryRow(`SELECT count(*) FROM drive_replacements WHERE session_id = ? AND name = ?`, sessionID, name).Scan(&existing); err != nil {
		return fmt.Errorf("check drive replacement %s/%s: %w", sessionID, name, err)
	}
	if existing != 0 {
		return fmt.Errorf("start drive replacement %s/%s: rows already exist", sessionID, name)
	}
	seen := make(map[string]bool, len(oldIDs))
	for _, oldID := range oldIDs {
		if oldID == "" || seen[oldID] {
			return fmt.Errorf("start drive replacement %s/%s: old ids must be non-empty and unique", sessionID, name)
		}
		seen[oldID] = true
		if _, err := tx.Exec(`INSERT INTO drive_replacements(session_id, name, old_id, new_id, state) VALUES(?, ?, ?, '', ?)`,
			sessionID, name, oldID, DriveReplacementUploading); err != nil {
			return fmt.Errorf("record drive replacement %s/%s: %w", sessionID, name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit drive replacement %s/%s: %w", sessionID, name, err)
	}
	return nil
}

func (s *SQLiteStore) MarkDriveReplacementUploaded(sessionID, name, newID string) error {
	if newID == "" {
		return fmt.Errorf("mark drive replacement uploaded %s/%s: new id is required", sessionID, name)
	}
	result, err := s.db.Exec(`UPDATE drive_replacements SET new_id = ?, state = ? WHERE session_id = ? AND name = ? AND state = ?`,
		newID, DriveReplacementUploaded, sessionID, name, DriveReplacementUploading)
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

func (s *SQLiteStore) MarkDriveReplacementDeleted(sessionID, name, oldID string) error {
	result, err := s.db.Exec(`UPDATE drive_replacements SET state = ? WHERE session_id = ? AND name = ? AND old_id = ? AND state = ?`,
		DriveReplacementDeleted, sessionID, name, oldID, DriveReplacementUploaded)
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

// FinishDriveReplacement clears the pending marker only once every old file
// recorded for the replacement is gone and the deletion generation is still
// the one that initiated this replacement.
func (s *SQLiteStore) FinishDriveReplacement(sessionID string, generation int) error {
	var count, incomplete int
	if err := s.db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE state != ?) FROM drive_replacements WHERE session_id = ?`, DriveReplacementDeleted, sessionID).Scan(&count, &incomplete); err != nil {
		return fmt.Errorf("check drive replacement for session %s: %w", sessionID, err)
	}
	if count < 3 || incomplete != 0 {
		return fmt.Errorf("finish drive replacement for session %s: %d rows, %d not deleted", sessionID, count, incomplete)
	}
	result, err := s.db.Exec(`UPDATE sessions SET drive_replace_pending = 0
		WHERE id = ? AND drive_replace_pending = 1 AND deletion_generation = ?`, sessionID, generation)
	if err != nil {
		return fmt.Errorf("finish drive replacement for session %s: %w", sessionID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count completed drive replacement for session %s: %w", sessionID, err)
	}
	if rows == 0 {
		return fmt.Errorf("finish drive replacement for session %s: %w", sessionID, sql.ErrNoRows)
	}
	return nil
}
