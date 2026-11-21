package storage

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// LatestRecordingSHA256 returns the latest post-splice hash for recording in
// sessionID. A matching hash is required before a Drive restore can replace a
// cut recording on the unit.
func (s *SQLiteStore) LatestRecordingSHA256(sessionID, recording string) (string, bool, error) {
	rows, err := s.db.Query(`SELECT audio_path, sha256_after FROM deleted_segments
		WHERE session_id = ? ORDER BY rowid DESC`, sessionID)
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

// RefinementRecordingSource identifies a source session recorded with a
// merged session's provenance.
type RefinementRecordingSource struct {
	ID                     string
	GDriveFolderID         string
	FallbackGDriveFolderID string
	RecordingPaths         []string
}

// RefinementRecordingSources follows recording provenance through nested
// merges to the original source session that owns each recording and Drive
// folder. Without provenance, a restore must refuse rather than guess.
func (s *SQLiteStore) RefinementRecordingSources(sessionID string) ([]RefinementRecordingSource, error) {
	rows, err := s.db.Query(`WITH RECURSIVE source(recording_path, session_id, folder_session_id) AS (
			SELECT recording_path, source_session_id, source_session_id
			FROM recording_sources WHERE merged_session_id = ?
			UNION ALL
			SELECT source.recording_path, nested.source_session_id,
				CASE WHEN parent.gdrive_folder_id != '' THEN source.session_id ELSE source.folder_session_id END
			FROM source
			JOIN recording_sources AS nested
			  ON nested.merged_session_id = source.session_id
			 AND nested.recording_path = source.recording_path
			JOIN sessions AS parent ON parent.id = source.session_id
		)
		SELECT source.session_id, folders.gdrive_folder_id, leaves.gdrive_folder_id, source.recording_path
		FROM source
		JOIN sessions AS folders ON folders.id = source.folder_session_id
		JOIN sessions AS leaves ON leaves.id = source.session_id
		WHERE NOT EXISTS (
			SELECT 1 FROM recording_sources AS nested
			WHERE nested.merged_session_id = source.session_id
			  AND nested.recording_path = source.recording_path
		)
		ORDER BY folders.started_at, source.recording_path`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list recording sources for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()
	byID := make(map[string]int)
	var sources []RefinementRecordingSource
	for rows.Next() {
		var id, folderID, fallbackFolderID, path string
		if err := rows.Scan(&id, &folderID, &fallbackFolderID, &path); err != nil {
			return nil, fmt.Errorf("scan recording source for session %s: %w", sessionID, err)
		}
		index, found := byID[id]
		if !found {
			index = len(sources)
			byID[id] = index
			sources = append(sources, RefinementRecordingSource{ID: id, GDriveFolderID: folderID, FallbackGDriveFolderID: fallbackFolderID})
		}
		sources[index].RecordingPaths = append(sources[index].RecordingPaths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recording sources for session %s: %w", sessionID, err)
	}
	return sources, nil
}

// RefinementRecordingPaths returns the recordings whose content belongs to a
// session. Merged sessions retain their source paths even when their current
// audio path was cleared by garbage collection; a session restored from Drive
// has the names its folder held; other refined sessions retain paths in their
// current stored word occurrences.
func (s *SQLiteStore) RefinementRecordingPaths(sessionID string) ([]string, error) {
	paths, err := knownRecordingPaths(s.db, sessionID)
	if err != nil {
		return nil, err
	}
	if len(paths) != 0 {
		return paths, nil
	}
	var merged int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE merged_into = ?`, sessionID).Scan(&merged); err != nil {
		return nil, fmt.Errorf("check merged recording provenance for session %s: %w", sessionID, err)
	}
	if merged != 0 {
		return nil, fmt.Errorf("recording provenance is unavailable for merged session %s", sessionID)
	}
	return nil, nil
}

// knownRecordingPaths returns the recordings whose content belongs to a
// session, from the first of its provenance sources that names any: the
// recordings a merge took from its sources, those a Drive restore found, and
// those its refined words at the refined revision came from. With none it
// returns nothing; each caller has its own rule for that.
func knownRecordingPaths(q querier, sessionID string) ([]string, error) {
	for _, source := range []struct{ what, query string }{
		{"merged recording sources", `SELECT recording_path FROM recording_sources
			WHERE merged_session_id = ? ORDER BY recording_path`},
		{"restored recordings", `SELECT recording_path FROM restored_recordings
			WHERE session_id = ? ORDER BY recording_path`},
		{"refined recording paths", `SELECT DISTINCT recording_path FROM refined_words
			WHERE session_id = ?1 AND transcript_revision = (SELECT refined_revision FROM sessions WHERE id = ?1)
			ORDER BY recording_path`},
	} {
		paths, err := refinementRecordingPaths(q, source.query, sessionID)
		if err != nil {
			return nil, fmt.Errorf("list %s for session %s: %w", source.what, sessionID, err)
		}
		if len(paths) != 0 {
			return paths, nil
		}
	}
	return nil, nil
}

func refinementRecordingPaths(q querier, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

// RestoreAudioForRefinement records recordings restored from Drive so the
// normal generation-fenced refinement path can claim and process them. It
// writes only when audioPath remains the path read before the restoration.
// The refinement becomes pending, unless it is running: a merge claims its
// refinement before it restores a removed recording (onSessionMerged), so a
// stop between the restore and the refinement leaves it running, which the
// next start retries (RecoverStaleRefinements).
func (s *SQLiteStore) RestoreAudioForRefinement(id, beforeAudioPath, restoredAudioPath string) (bool, error) {
	if strings.TrimSpace(restoredAudioPath) == "" {
		return false, errors.New("restored audio path is required")
	}
	res, err := s.db.Exec(`UPDATE sessions SET audio_path = ?, audio_removed_at = '',
			refinement_status = CASE WHEN refinement_status = ? THEN refinement_status ELSE ? END
		WHERE id = ? AND audio_path = ?`,
		restoredAudioPath, RefinementRunning, RefinementPending, id, beforeAudioPath)
	if err != nil {
		return false, fmt.Errorf("record restored audio for session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count restored audio session %s: %w", id, err)
	}
	return rows > 0, nil
}
