package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// TextDeletionSpan is where, in one recording, the words of a segment that
// "Delete text" deleted are: Start to End seconds of recording AudioPath,
// whose bytes then had SHA-256 SHA256. It holds no text. The recording keeps
// the words, so every later batch refinement drops the words it finds in a
// span (Sami, 2026-11-12, dispatch://INFRA-2961/ask/8dcb4d3f-a256-45dd-b0ac-ed4b06fe2a05).
type TextDeletionSpan struct {
	AudioPath  string
	Start, End float64
	SHA256     string
}

// TextDeleted reports whether a word heard from start to end seconds in a
// recording is one of the words "Delete text" removed from it, by the
// recording's spans: its middle is in one of them. A span is the deleted
// words' aligned span, so a neighbour's word, which does not overlap them,
// is not in it.
func TextDeleted(spans []TextDeletionSpan, start, end float64) bool {
	middle := (start + end) / 2
	for i := range spans {
		if middle >= spans[i].Start && middle <= spans[i].End {
			return true
		}
	}
	return false
}

func markDriveDocumentsPending(tx *sql.Tx, sessionID string, sources []string) error {
	for _, id := range append([]string{sessionID}, sources...) {
		if _, err := tx.Exec(`UPDATE sessions SET drive_documents_pending = 1 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("owe the Drive folder of session %s its documents: %w", id, err)
		}
	}
	return nil
}

// recordTextDeletionSpan records, in tx, the span of sessionID's recording
// that a "Delete text" deletion's words are in: the recording's file name,
// the times and the recording's SHA-256, never the words. And sessionID and
// each of sources (the sessions merged into it) that has a Drive folder of
// its own, whose documents the deletion changed, owes that folder a refresh
// of them in place (drive_documents_pending): the session's own sync does it
// and clears the mark (MarkSynced); a merged source, which no sync of its
// own reaches, is refreshed by every sync of the session it was merged into
// and by the sweep (DriveDocumentsOwed), also when the session the deletion
// was made in is merged before its sync ran.
func recordTextDeletionSpan(tx *sql.Tx, sessionID string, sources []string, span *DeletedSpan) error {
	if _, err := tx.Exec(`INSERT INTO deleted_text_spans(session_id, recording, start, end, sha256, deleted_at) VALUES(?, ?, ?, ?, ?, ?)`,
		sessionID, filepath.Base(strings.TrimSpace(span.AudioPath)), span.Start, span.End, span.SHA256Before, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record deleted text span for session %s: %w", sessionID, err)
	}
	return markDriveDocumentsPending(tx, sessionID, sources)
}

// DriveDocumentsOwed returns the sessions whose own Drive folder holds
// documents a "Delete text" deletion changed and not yet updated: session
// sessionID itself, if it is such a merged source, else those of the
// sessions merged, directly or through another merge, into it. It returns
// them in session time order.
func (s *SQLiteStore) DriveDocumentsOwed(sessionID string) ([]string, error) {
	candidates, err := mergedSourceIDs(s.db, sessionID)
	if err != nil {
		return nil, err
	}
	candidates = append([]string{sessionID}, candidates...)
	var owed []string
	for _, id := range candidates {
		var pending int
		err := s.db.QueryRow(`SELECT drive_documents_pending FROM sessions WHERE id = ? AND status = ? AND gdrive_folder_id != ''`, id, SessionMerged).Scan(&pending)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("read the owed Drive documents of session %s: %w", id, err)
		}
		if pending == 1 {
			owed = append(owed, id)
		}
	}
	return owed, nil
}

// ClearDriveDocumentsPending records that merged session id's Drive folder
// has its documents as they are now, only while unchanged reports true for
// the session as it is now, read in the same transaction, and reports
// whether it did. When the session changed, it marks the folder owed again:
// the refresh that read it before the change may have uploaded after a newer
// one cleared the mark, so only another refresh puts the current documents
// back. unchanged runs inside the transaction: it must not use the store.
func (s *SQLiteStore) ClearDriveDocumentsPending(id string, unchanged func(Session) bool) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin clearing the owed Drive documents of session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	sess, err := getSession(tx, id)
	if err != nil {
		return false, err
	}
	pending, cleared := 0, true
	if !unchanged(sess) {
		pending, cleared = 1, false
	}
	if _, err := tx.Exec(`UPDATE sessions SET drive_documents_pending = ? WHERE id = ?`, pending, id); err != nil {
		return false, fmt.Errorf("record the owed Drive documents of session %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit the owed Drive documents of session %s: %w", id, err)
	}
	return cleared, nil
}

// TextDeletionSpans returns the spans "Delete text" recorded in recording,
// found by its file name wherever it is kept and whichever session (the one
// it was recorded in, or one it was merged into) the deletion was made in,
// in time order. AudioPath is the recording's file name.
func (s *SQLiteStore) TextDeletionSpans(recording string) ([]TextDeletionSpan, error) {
	name := filepath.Base(strings.TrimSpace(recording))
	rows, err := s.db.Query(`SELECT recording, start, end, sha256 FROM deleted_text_spans WHERE recording = ? ORDER BY start, end, rowid`, name)
	if err != nil {
		return nil, fmt.Errorf("list deleted text spans of %s: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	var spans []TextDeletionSpan
	for rows.Next() {
		var span TextDeletionSpan
		if err := rows.Scan(&span.AudioPath, &span.Start, &span.End, &span.SHA256); err != nil {
			return nil, fmt.Errorf("scan deleted text span of %s: %w", name, err)
		}
		spans = append(spans, span)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deleted text spans of %s: %w", name, err)
	}
	return spans, nil
}

// moveTextDeletionSpans moves, in tx, the text deletion spans recorded in
// cut's recording as it was before the splice (its SHA-256 before) to where
// their words are once Start to End is cut out (audio.Splice joins what is
// before Start to what is after End): a span before the cut stays, one after
// it moves back by its length, the part of one inside it goes with the audio,
// and one wholly inside it goes. Each moved span names the spliced bytes.
func moveTextDeletionSpans(tx *sql.Tx, cut *DeletedSpan) error {
	name := filepath.Base(strings.TrimSpace(cut.AudioPath))
	length := cut.End - cut.Start
	moved := func(column string) string {
		return `CASE WHEN ` + column + ` <= ?1 THEN ` + column + ` WHEN ` + column + ` >= ?2 THEN ` + column + ` - ?3 ELSE ?1 END`
	}
	if _, err := tx.Exec(`UPDATE deleted_text_spans SET start = `+moved("start")+`, end = `+moved("end")+`, sha256 = ?4
		WHERE recording = ?5 AND sha256 = ?6`,
		cut.Start, cut.End, length, cut.SHA256After, name, cut.SHA256Before); err != nil {
		return fmt.Errorf("move deleted text spans of %s past its splice: %w", name, err)
	}
	if _, err := tx.Exec(`DELETE FROM deleted_text_spans WHERE recording = ? AND sha256 = ? AND end <= start`, name, cut.SHA256After); err != nil {
		return fmt.Errorf("drop deleted text spans cut out of %s: %w", name, err)
	}
	return nil
}
