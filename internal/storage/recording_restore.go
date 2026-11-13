package storage

import (
	"fmt"
	"path/filepath"
)

// LatestRecordingSHA256 returns the latest post-splice hash for recording in
// sessionID. A matching hash is required before a Drive restore can replace a
// cut recording on the unit.
func (s *SQLiteStore) LatestRecordingSHA256(sessionID, recording string) (string, bool, error) {
	rows, err := s.db.Query(`SELECT audio_path, sha256_after FROM deleted_segments
		WHERE session_id = ? ORDER BY deleted_at DESC, rowid DESC`, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("list deleted recording hashes for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()
	name := filepath.Base(recording)
	for rows.Next() {
		var path, hash string
		if err := rows.Scan(&path, &hash); err != nil {
			return "", false, fmt.Errorf("scan deleted recording hash for session %s: %w", sessionID, err)
		}
		if filepath.Base(path) == name {
			return hash, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("iterate deleted recording hashes for session %s: %w", sessionID, err)
	}
	return "", false, nil
}
