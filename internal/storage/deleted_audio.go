package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/recording"
)

// DeletedSessionAudio is a session a person's delete or GC's age delete took,
// recorded in the transaction that deleted its row, and the audio path the
// row named then. The kept recordings pass erases the audio files of such a
// session that a stop left behind, then clears the record.
type DeletedSessionAudio struct {
	SessionID string
	AudioPath string
}

// ErrSessionAudioPending refuses a restore or a merge that would bring back
// an id whose recorded delete the kept recordings pass has not cleared yet:
// the pass erases the audio files of that id, the files a restored session's
// Retry refinement downloads included. It clears the record once the files
// are gone, at the latest at its next run.
var ErrSessionAudioPending = errors.New("the audio of a deleted session with this id is still being erased; try again in a few minutes")

// queryRower is what deletedAudioPending reads through: a transaction or the
// database.
type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// deletedAudioPending reports whether a recorded delete of session id is
// waiting for the kept recordings pass.
func deletedAudioPending(q queryRower, id string) (bool, error) {
	var pending int
	switch err := q.QueryRow(`SELECT 1 FROM deleted_session_audio WHERE session_id = ?`, id).Scan(&pending); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check for a pending delete of session %s: %w", id, err)
	}
	return true, nil
}

// NamedAudio returns every audio file name a session names: each entry of
// every session's audio path, and each recording a restored session's Drive
// folder holds (restored_recordings), which a merge or a Retry refinement
// names for that session later. Nothing that erases a deleted session's
// audio, or gives a kept recording to a session, may touch one of these.
func (s *SQLiteStore) NamedAudio() ([]string, error) {
	rows, err := s.db.Query(`SELECT audio_path FROM sessions WHERE audio_path != ''
		UNION ALL SELECT recording_path FROM restored_recordings`)
	if err != nil {
		return nil, fmt.Errorf("list the audio sessions name: %w", err)
	}
	var named []string
	for rows.Next() {
		var audioPath string
		if err := rows.Scan(&audioPath); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan the audio a session names: %w", err)
		}
		named = append(named, recording.Entries(audioPath)...)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list the audio sessions name: %w", err)
	}
	return named, nil
}

// recordDeletedSessionAudio records, in tx, the sessions about to be deleted
// there, each with the audio its row names and the recordings its restored
// Drive folder holds (restored_recordings), which the row's delete drops with
// it. Run it before the rows go.
func recordDeletedSessionAudio(tx *sql.Tx, sessionIDs []string) error {
	for _, id := range sessionIDs {
		var audioPath string
		switch err := tx.QueryRow(`SELECT audio_path FROM sessions WHERE id = ?`, id).Scan(&audioPath); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("read the audio of deleted session %s: %w", id, err)
		}
		restored, err := restoredRecordingNames(tx, []string{id})
		if err != nil {
			return err
		}
		entries := append(recording.Entries(audioPath), restored...)
		if _, err := tx.Exec(`INSERT INTO deleted_session_audio(session_id, audio_path) VALUES(?, ?)
			ON CONFLICT(session_id) DO UPDATE SET audio_path = excluded.audio_path`, id, strings.Join(entries, ",")); err != nil {
			return fmt.Errorf("record the audio of deleted session %s: %w", id, err)
		}
	}
	return nil
}

// restoredRecordingNames returns, in tx, the recordings the restored Drive
// folders of sessionIDs hold (restored_recordings).
func restoredRecordingNames(tx *sql.Tx, sessionIDs []string) ([]string, error) {
	var names []string
	for _, id := range sessionIDs {
		rows, err := tx.Query(`SELECT recording_path FROM restored_recordings WHERE session_id = ? ORDER BY recording_path`, id)
		if err != nil {
			return nil, fmt.Errorf("list the restored recordings of session %s: %w", id, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan a restored recording of session %s: %w", id, err)
			}
			names = append(names, name)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("list the restored recordings of session %s: %w", id, err)
		}
	}
	return names, nil
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
