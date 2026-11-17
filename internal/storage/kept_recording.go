package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// audioOwnedSinceKey is the app_state key that records from when the audio
// directory's files belong to this database (AudioOwnedSince).
const audioOwnedSinceKey = "audio_owned_since"

// AudioOwnedSince returns from when the files in the audio directory belong
// to this database: the first time a build that records it opened it. A file
// last written before then may be a recording of a database that was lost
// and made again, or one an earlier build left, so the absence of its
// session's row does not mean the session was deleted.
func (s *SQLiteStore) AudioOwnedSince() (time.Time, error) {
	var value string
	if err := s.db.QueryRow(`SELECT value FROM app_state WHERE key = ?`, audioOwnedSinceKey).Scan(&value); err != nil {
		return time.Time{}, fmt.Errorf("read from when the audio belongs to this database: %w", err)
	}
	since, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("read from when the audio belongs to this database: %w", err)
	}
	return since, nil
}

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
//
// place, when not nil, puts the recording at audioPath (a rename from the
// temporary name it was saved under). It runs inside the transaction, once
// the session is found to take the recording and before it is recorded, so a
// session deleted meanwhile never gets a file under audioPath: a delete waits
// for the transaction, and a refused session leaves the file where it was
// saved. If place fails, nothing is recorded. If the transaction fails after
// place, the file is at audioPath and named by no session.
func (s *SQLiteStore) RecordKeptRecording(id, audioPath string, place func() error) (bool, error) {
	if strings.TrimSpace(audioPath) == "" {
		return false, errors.New("the kept recording's path is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin recording the kept recording of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var takes int
	switch err := tx.QueryRow(`SELECT 1 FROM sessions WHERE id = ? AND status = ? AND audio_path = '' AND audio_removed_at = ''`,
		id, SessionEnded).Scan(&takes); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read session %s to record its kept recording: %w", id, err)
	}
	if place != nil {
		if err := place(); err != nil {
			return false, fmt.Errorf("put the kept recording of session %s at %s: %w", id, audioPath, err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE sessions SET audio_path = ?,
			refinement_status = CASE WHEN refinement_status = ? THEN ? ELSE refinement_status END,
			sync_status = CASE WHEN sync_state = ? THEN ? ELSE sync_status END,
			sync_state = CASE WHEN sync_state = ? THEN ? ELSE sync_state END
		 WHERE id = ?`,
		audioPath,
		RefinementNoAudio, RefinementPending,
		SyncStateSynced, SyncPending,
		SyncStateSynced, SyncStatePending,
		id,
	); err != nil {
		return false, fmt.Errorf("record the kept recording of session %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit the kept recording of session %s: %w", id, err)
	}
	return true, nil
}
