package storage

import (
	"errors"
	"fmt"
	"strings"
)

// RecordKeptRecording records audioPath, saved from the raw recording kept
// for session id, as the session's recording, and reports whether it did. It
// does so only while the session is ended with no recording: the app stopped
// inside it (RecoverStaleSessions ended it) or every save at its end failed.
// A session whose audio GC removed to free space is left as it is, so a raw
// recording left beside that audio does not bring it back. The session's
// refinement can then run (no_audio becomes pending), and a synced session
// goes back to pending sync, keeping its Drive folder, so Drive gets the
// recording. Any other sync state is left as it is: a REMOTE_DELETED session
// is uploaded to no folder the user deleted.
func (s *SQLiteStore) RecordKeptRecording(id, audioPath string) (bool, error) {
	if strings.TrimSpace(audioPath) == "" {
		return false, errors.New("the kept recording's path is required")
	}
	res, err := s.db.Exec(
		`UPDATE sessions SET audio_path = ?,
			refinement_status = CASE WHEN refinement_status = ? THEN ? ELSE refinement_status END,
			sync_status = CASE WHEN sync_state = ? THEN ? ELSE sync_status END,
			sync_state = CASE WHEN sync_state = ? THEN ? ELSE sync_state END
		 WHERE id = ? AND status = ? AND audio_path = '' AND audio_removed_at = ''`,
		audioPath,
		RefinementNoAudio, RefinementPending,
		SyncStateSynced, SyncPending,
		SyncStateSynced, SyncStatePending,
		id, SessionEnded,
	)
	if err != nil {
		return false, fmt.Errorf("record the kept recording of session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record the kept recording of session %s: %w", id, err)
	}
	return rows == 1, nil
}
