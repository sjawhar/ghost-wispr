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
// its new timeline, in the splice's transaction, as moveRefinedWordsAfterSplice
// moves the words: a time before the splice stays, a time inside it goes to
// its start, and a time after it moves back by its length. A refused cut the
// splice covers is gone with its words; one it overlaps, such as one the pad
// of a touching neighbour's cut reaches into, is trimmed to what is left. A
// recording's file name is the session's id, so it names the recording in
// whichever session the cut was refused in. The rows are rewritten together,
// so two refused cuts that end up with one span keep one row.
func moveRefusedCutsAfterSplice(tx *sql.Tx, span *DeletedSpan) error {
	recording := filepath.Base(span.AudioPath)
	rows, err := tx.Query(`SELECT session_id, start, end, refused_at FROM refused_cuts WHERE recording = ? ORDER BY refused_at, rowid`, recording)
	if err != nil {
		return fmt.Errorf("read refused cuts of %s for its splice: %w", recording, err)
	}
	type kept struct {
		cut       RefusedCut
		refusedAt string
	}
	var cuts []kept
	for rows.Next() {
		k := kept{cut: RefusedCut{Recording: recording}}
		if err := rows.Scan(&k.cut.SessionID, &k.cut.Start, &k.cut.End, &k.refusedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan refused cut of %s: %w", recording, err)
		}
		cuts = append(cuts, k)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read refused cuts of %s for its splice: %w", recording, err)
	}
	if len(cuts) == 0 {
		return nil
	}
	move := func(t float64) float64 {
		switch {
		case t <= span.Start:
			return t
		case t >= span.End:
			return t - (span.End - span.Start)
		default:
			return span.Start
		}
	}
	if _, err := tx.Exec(`DELETE FROM refused_cuts WHERE recording = ?`, recording); err != nil {
		return fmt.Errorf("clear refused cuts of %s for its splice: %w", recording, err)
	}
	for _, k := range cuts {
		start, end := move(k.cut.Start), move(k.cut.End)
		if end <= start {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO refused_cuts(session_id, recording, start, end, refused_at) VALUES(?, ?, ?, ?, ?)`,
			k.cut.SessionID, recording, start, end, k.refusedAt); err != nil {
			return fmt.Errorf("move refused cut of %s past its splice: %w", recording, err)
		}
	}
	return nil
}
