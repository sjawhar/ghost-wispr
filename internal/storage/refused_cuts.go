package storage

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"
)

// RefusedCut is a "Delete text and audio" the check after the cut refused,
// so the segment's words are still in the recording: the session it was
// refused in, the recording's file name and the segment's time in it, never
// its words, which the person asked to delete. It is kept until the words are
// gone, so the status page can say so across restarts.
type RefusedCut struct {
	SessionID  string
	Recording  string // the recording's file name
	Start, End float64
}

// RecordRefusedCut keeps a refused cut. Recording the same cut again keeps
// one.
func (s *SQLiteStore) RecordRefusedCut(cut RefusedCut) error {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO refused_cuts(session_id, recording, start, end, refused_at) VALUES(?, ?, ?, ?, ?)`,
		cut.SessionID, filepath.Base(cut.Recording), cut.Start, cut.End, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record refused cut in session %s: %w", cut.SessionID, err)
	}
	return nil
}

// RefusedCuts returns the refused cuts kept, oldest first.
func (s *SQLiteStore) RefusedCuts() ([]RefusedCut, error) {
	rows, err := s.db.Query(`SELECT session_id, recording, start, end FROM refused_cuts ORDER BY refused_at, rowid`)
	if err != nil {
		return nil, fmt.Errorf("read refused cuts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var cuts []RefusedCut
	for rows.Next() {
		var cut RefusedCut
		if err := rows.Scan(&cut.SessionID, &cut.Recording, &cut.Start, &cut.End); err != nil {
			return nil, fmt.Errorf("scan refused cut: %w", err)
		}
		cuts = append(cuts, cut)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read refused cuts: %w", err)
	}
	return cuts, nil
}

// ForgetRefusedCut drops a refused cut whose segment's words are gone.
func (s *SQLiteStore) ForgetRefusedCut(cut RefusedCut) error {
	if _, err := s.db.Exec(`DELETE FROM refused_cuts WHERE session_id = ? AND recording = ? AND start = ? AND end = ?`,
		cut.SessionID, filepath.Base(cut.Recording), cut.Start, cut.End); err != nil {
		return fmt.Errorf("forget refused cut in session %s: %w", cut.SessionID, err)
	}
	return nil
}

// moveRefusedCutsAfterSplice keeps the refused cuts of a spliced recording in
// its new timeline, in the splice's transaction: a refused cut the splice
// overlaps is gone with its words, and one after it moves back by its length.
// A recording's file name is the session's id, so it names the recording in
// whichever session the cut was refused in.
func moveRefusedCutsAfterSplice(tx *sql.Tx, span *DeletedSpan) error {
	recording := filepath.Base(span.AudioPath)
	if _, err := tx.Exec(`DELETE FROM refused_cuts WHERE recording = ? AND end > ? AND start < ?`, recording, span.Start, span.End); err != nil {
		return fmt.Errorf("drop refused cuts a splice removed: %w", err)
	}
	if _, err := tx.Exec(`UPDATE refused_cuts SET start = start - ?1, end = end - ?1 WHERE recording = ?2 AND start >= ?3`,
		span.End-span.Start, recording, span.End); err != nil {
		return fmt.Errorf("move refused cuts after splice: %w", err)
	}
	return nil
}
