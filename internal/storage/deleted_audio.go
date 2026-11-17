package storage

import (
	"database/sql"
	"errors"
	"fmt"
)

// DeletedSessionAudio is a session a person's delete or GC's age delete took,
// recorded in the transaction that deleted its row, and the audio path the
// row named then. The kept recordings pass erases the audio files of such a
// session that a stop left behind, then clears the record.
type DeletedSessionAudio struct {
	SessionID string
	AudioPath string
}

// recordDeletedSessionAudio records, in tx, the sessions about to be deleted
// there, with the audio path each row names. Run it before the rows go.
func recordDeletedSessionAudio(tx *sql.Tx, sessionIDs []string) error {
	for _, id := range sessionIDs {
		if _, err := tx.Exec(`INSERT INTO deleted_session_audio(session_id, audio_path)
			SELECT id, audio_path FROM sessions WHERE id = ?
			ON CONFLICT(session_id) DO UPDATE SET audio_path = excluded.audio_path`, id); err != nil {
			return fmt.Errorf("record the audio of deleted session %s: %w", id, err)
		}
	}
	return nil
}

// DeletedSessionAudio lists, by session id, the recorded deleted sessions
// whose audio the kept recordings pass has not cleared yet.
func (s *SQLiteStore) DeletedSessionAudio() ([]DeletedSessionAudio, error) {
	rows, err := s.db.Query(`SELECT session_id, audio_path FROM deleted_session_audio ORDER BY session_id`)
	if err != nil {
		return nil, fmt.Errorf("list deleted session audio: %w", err)
	}
	var records []DeletedSessionAudio
	for rows.Next() {
		var record DeletedSessionAudio
		if err := rows.Scan(&record.SessionID, &record.AudioPath); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan deleted session audio: %w", err)
		}
		records = append(records, record)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list deleted session audio: %w", err)
	}
	return records, nil
}

// ClearDeletedSessionAudio forgets the record of deleted session id, once
// none of its audio files is left.
func (s *SQLiteStore) ClearDeletedSessionAudio(id string) error {
	if _, err := s.db.Exec(`DELETE FROM deleted_session_audio WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("clear the audio record of deleted session %s: %w", id, err)
	}
	return nil
}
