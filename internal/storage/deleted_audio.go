package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/recording"
)

// DeletedSessionAudio is a session a person's delete or GC's age delete took,
// recorded in the transaction that deleted its row, and the files of it the
// kept recordings pass erases if a stop cuts the delete's own erase short:
// absolute paths, fixed at the delete. A person's delete lists every audio
// file of the session (deleteFiles); GC's age delete lists only what it
// checked on Drive or erases itself as redundant (ageDeleteFiles). The pass
// erases the listed files a stop left, except one a session names, then
// clears the record.
type DeletedSessionAudio struct {
	SessionID string
	Files     []string
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

// restoredRecordingFile returns the file in audioDir that name, a recording
// a restored session's Drive folder holds (restored_recordings), names, and
// whether it names one. Such a name comes from Drive: it names a file only
// when it is the plain name of a recording (an .mp3 or .wav, with no
// directory, not hidden, and no comma, which an audio path would split), and
// it is always taken in audioDir, never resolved against the working
// directory.
func restoredRecordingFile(audioDir, name string) (string, bool) {
	if audioDir == "" || name == "" || filepath.Base(name) != name || strings.HasPrefix(name, ".") || strings.ContainsRune(name, ',') {
		return "", false
	}
	switch filepath.Ext(name) {
	case ".mp3", ".wav":
	default:
		return "", false
	}
	return filepath.Join(audioDir, name), true
}

// NamedAudio returns, absolute, every audio file a session names: each entry
// of every session's audio path, found where recording.Resolve finds it in
// audioDir, and each recording a restored session's Drive folder holds
// (restored_recordings, restoredRecordingFile), which a merge or a Retry
// refinement names for that session later. Nothing that erases a deleted
// session's audio, or gives a kept recording to a session, may touch one of
// these.
func (s *SQLiteStore) NamedAudio(audioDir string) ([]string, error) {
	var named []string
	add := func(path string) {
		if abs, err := filepath.Abs(path); err == nil {
			named = append(named, abs)
		}
	}
	rows, err := s.db.Query(`SELECT audio_path FROM sessions WHERE audio_path != ''`)
	if err != nil {
		return nil, fmt.Errorf("list the audio sessions name: %w", err)
	}
	for rows.Next() {
		var audioPath string
		if err := rows.Scan(&audioPath); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan the audio a session names: %w", err)
		}
		for _, entry := range recording.Entries(audioPath) {
			add(recording.Resolve(entry, audioDir))
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list the audio sessions name: %w", err)
	}
	restored, err := s.db.Query(`SELECT recording_path FROM restored_recordings`)
	if err != nil {
		return nil, fmt.Errorf("list the recordings of restored sessions: %w", err)
	}
	for restored.Next() {
		var name string
		if err := restored.Scan(&name); err != nil {
			_ = restored.Close()
			return nil, fmt.Errorf("scan a recording of a restored session: %w", err)
		}
		if path, ok := restoredRecordingFile(audioDir, name); ok {
			add(path)
		}
	}
	if err := errors.Join(restored.Err(), restored.Close()); err != nil {
		return nil, fmt.Errorf("list the recordings of restored sessions: %w", err)
	}
	return named, nil
}

// deleteFiles returns, absolute and once each, every audio file of session id
// in audioDir that a person's delete erases: each recording its audio path
// names (found where recording.Resolve finds it), each recording its restored
// Drive folder holds (restoredRecordingFile), its raw capture, <id>.mp3 and
// <id>.wav, and a save's temporary files.
func deleteFiles(tx *sql.Tx, id, audioPath, audioDir string) ([]string, error) {
	var files []string
	for _, entry := range recording.Entries(audioPath) {
		files = append(files, recording.Resolve(entry, audioDir))
	}
	names, err := restoredRecordingNames(tx, id)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if path, ok := restoredRecordingFile(audioDir, name); ok {
			files = append(files, path)
		}
	}
	if audioDir != "" {
		files = append(files,
			recording.RawPath(audioDir, id),
			filepath.Join(audioDir, id+".mp3"),
			filepath.Join(audioDir, id+".wav"))
		files = append(files, recording.KeptTempPaths(audioDir, id)...)
	}
	return absoluteOnce(files), nil
}

// ageDeleteFiles returns, absolute and once each, the audio files of session
// id in audioDir that GC's age delete erases: each recording its audio path
// names, which GC checked on Drive, and its raw capture, which GC erases as a
// redundant copy. A file Drive was not asked about (a <id>.wav beside the
// recording, a restored folder's recording) is not listed: it stays.
func ageDeleteFiles(id, audioPath, audioDir string) []string {
	var files []string
	for _, entry := range recording.Entries(audioPath) {
		files = append(files, recording.Resolve(entry, audioDir))
	}
	if audioDir != "" {
		files = append(files, recording.RawPath(audioDir, id))
	}
	return absoluteOnce(files)
}

// absoluteOnce returns paths absolute, in order, each once.
func absoluteOnce(paths []string) []string {
	var files []string
	seen := map[string]bool{}
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		files = append(files, abs)
	}
	return files
}

// recordDeletedSessionAudio records, in tx, the sessions about to be deleted
// there, each with the files files lists for it from its audio path. Run it
// before the rows go.
func recordDeletedSessionAudio(tx *sql.Tx, sessionIDs []string, files func(id, audioPath string) ([]string, error)) error {
	for _, id := range sessionIDs {
		var audioPath string
		switch err := tx.QueryRow(`SELECT audio_path FROM sessions WHERE id = ?`, id).Scan(&audioPath); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("read the audio of deleted session %s: %w", id, err)
		}
		listed, err := files(id, audioPath)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(listed)
		if err != nil {
			return fmt.Errorf("record the audio of deleted session %s: %w", id, err)
		}
		if _, err := tx.Exec(`INSERT INTO deleted_session_audio(session_id, files) VALUES(?, ?)
			ON CONFLICT(session_id) DO UPDATE SET files = excluded.files`, id, string(encoded)); err != nil {
			return fmt.Errorf("record the audio of deleted session %s: %w", id, err)
		}
	}
	return nil
}

// restoredRecordingNames returns, in tx, the names of the recordings the
// restored Drive folder of session id holds (restored_recordings).
func restoredRecordingNames(tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.Query(`SELECT recording_path FROM restored_recordings WHERE session_id = ? ORDER BY recording_path`, id)
	if err != nil {
		return nil, fmt.Errorf("list the restored recordings of session %s: %w", id, err)
	}
	var names []string
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
	return names, nil
}

// DeletedSessionAudio lists, by session id, the recorded deleted sessions
// whose audio the kept recordings pass has not cleared yet.
func (s *SQLiteStore) DeletedSessionAudio() ([]DeletedSessionAudio, error) {
	rows, err := s.db.Query(`SELECT session_id, files FROM deleted_session_audio ORDER BY session_id`)
	if err != nil {
		return nil, fmt.Errorf("list deleted session audio: %w", err)
	}
	var records []DeletedSessionAudio
	for rows.Next() {
		var record DeletedSessionAudio
		var encoded string
		if err := rows.Scan(&record.SessionID, &encoded); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan deleted session audio: %w", err)
		}
		if err := json.Unmarshal([]byte(encoded), &record.Files); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read the files of deleted session %s: %w", record.SessionID, err)
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
