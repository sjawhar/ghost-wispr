package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// DeleteRefinedSegmentText removes exactly the persisted word occurrences of
// a batch-derived segment. It never tries to find the displayed text in a new
// transcription.
func (s *SQLiteStore) DeleteRefinedSegmentText(sessionID string, segmentID int64, revision int) error {
	return s.deleteRefinedSegment(sessionID, segmentID, revision, nil, true)
}

// DeleteRefinedSegmentAudio records an occurrence-based deletion before the
// caller atomically replaces the recording with span's already-spliced bytes.
func (s *SQLiteStore) DeleteRefinedSegmentAudio(sessionID string, segmentID int64, revision int, span *DeletedSpan) error {
	if span == nil {
		return fmt.Errorf("delete refined segment: missing audio span")
	}
	return s.deleteRefinedSegment(sessionID, segmentID, revision, span, false)
}

func (s *SQLiteStore) deleteRefinedSegment(sessionID string, segmentID int64, revision int, span *DeletedSpan, keepAudio bool) error {
	if segmentID <= 0 {
		return ErrSegmentNotFound
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin refined segment deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	session, err := getSession(tx, sessionID)
	if err != nil {
		return err
	}
	if session.Status == SessionActive {
		return ErrSegmentSessionActive
	}
	if session.RefinementStatus != RefinementCompleted || session.RefinedRevision == 0 {
		return ErrRefinedSegmentsUnavailable
	}
	if revision != session.RefinedRevision {
		return ErrStaleTranscriptRevision
	}

	selected, err := refinedSegment(tx, sessionID, segmentID, revision)
	if err != nil {
		return err
	}
	if keepAudio {
		span = &DeletedSpan{
			SessionID: sessionID, AudioPath: selected.RecordingPath,
			Start: selected.Start, End: selected.End,
			SHA256Before: selected.RecordingSHA256, SHA256After: selected.RecordingSHA256,
		}
	} else {
		if span == nil || filepath.Base(span.AudioPath) != filepath.Base(selected.RecordingPath) ||
			span.SHA256Before != selected.RecordingSHA256 || span.Start > selected.Start || span.End < selected.End || span.End <= span.Start {
			return fmt.Errorf("delete refined segment: span does not cover the stored word occurrence")
		}
		span.SessionID = sessionID
	}
	if _, err := tx.Exec(`DELETE FROM refined_words
		WHERE session_id = ? AND transcript_revision = ? AND recording_path = ? AND recording_sha256 = ?
		  AND ordinal BETWEEN ? AND ?`,
		sessionID, revision, selected.RecordingPath, selected.RecordingSHA256, selected.FirstWordOrdinal, selected.LastWordOrdinal); err != nil {
		return fmt.Errorf("remove selected refined words: %w", err)
	}
	words, err := refinedWords(tx, sessionID, revision)
	if err != nil {
		return err
	}
	nextRevision := revision + 1
	if _, err := tx.Exec(`UPDATE refined_words SET transcript_revision = ? WHERE session_id = ? AND transcript_revision = ?`, nextRevision, sessionID, revision); err != nil {
		return fmt.Errorf("advance refined word revision: %w", err)
	}
	if err := replaceRefinedSegments(tx, sessionID, nextRevision, session.StartedAt, words); err != nil {
		return err
	}
	remaining, err := getSegments(tx, sessionID)
	if err != nil {
		return err
	}
	transcript := RefinedTranscript(words)
	if _, err := tx.Exec(`UPDATE sessions
		SET title = '', summary = '', summary_model = '', speaker_names = '{}',
			refined_transcript = ?, refinement_status = ?, canonical_transcript = ?, transcript_source = ?,
			summary_status = ?, summary_error = '', summary_version = summary_version + 1,
			deletion_generation = deletion_generation + 1,
			refinement_generation = deletion_generation + 1,
			refined_revision = ?,
			envoy_publish_status = ?,
			sync_status = CASE WHEN sync_state IN (?, ?) THEN sync_status ELSE ? END,
			sync_state = CASE WHEN sync_state IN (?, ?) THEN sync_state ELSE ? END
		WHERE id = ? AND refined_revision = ?`,
		transcript, RefinementCompleted, transcript, TranscriptSourceRefined,
		SummaryPending, nextRevision, EnvoyPublishPending,
		SyncStateSyncing, SyncStateRemoteDeleted, SyncPending,
		SyncStateSyncing, SyncStateRemoteDeleted, SyncStatePending,
		sessionID, revision); err != nil {
		return fmt.Errorf("update refined segment deletion session: %w", err)
	}
	sources, holders, err := scrubMergedSourcesByRecording(tx, sessionID, selected.RecordingPath, remaining)
	if err != nil {
		return err
	}
	if len(sources) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sources)), ",")
		args := make([]any, len(sources))
		for i, source := range sources {
			args[i] = source
		}
		if _, err := tx.Exec(`DELETE FROM refined_words WHERE session_id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("drop source refined words after merged deletion: %w", err)
		}
	}
	owners := append([]string{sessionID}, sources...)
	if err := dropLiveTranscriptEvents(tx, owners); err != nil {
		return err
	}
	for _, owner := range owners {
		if _, err := tx.Exec(`DELETE FROM embeddings WHERE session_id = ?`, owner); err != nil {
			return fmt.Errorf("clear embeddings for session %s: %w", owner, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO summary_requests(session_id, prompt_hash) VALUES(?, 'segment-delete')
		ON CONFLICT(session_id, prompt_hash) DO UPDATE SET created_at = CURRENT_TIMESTAMP`, sessionID); err != nil {
		return fmt.Errorf("queue summary for session %s: %w", sessionID, err)
	}
	if keepAudio {
		if err := recordTextDeletionSpan(tx, sessionID, sources, span); err != nil {
			return err
		}
	} else if err := recordSplice(tx, sessionID, owners, span, session.AudioPath); err != nil {
		return err
	}
	owed, earlierOwed, err := recordOwedErase(tx)
	if err != nil {
		return err
	}
	if err := rewriteDeletedTextEvents(tx, owners, append([]string{sessionID}, holders...), selected.Text, span); err != nil {
		return fmt.Errorf("rewrite events for session %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild search after deleting refined segment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit refined text deletion: %w", err)
	}
	return s.eraseAfterDeletion(owed, earlierOwed, segmentID, selected.Text, owners, owners, span)
}

func refinedSegment(q querier, sessionID string, segmentID int64, revision int) (RefinedSegment, error) {
	var segment RefinedSegment
	err := q.QueryRow(`SELECT text, recording_path, recording_sha256, recording_ordinal,
		first_word_ordinal, last_word_ordinal, speaker, start_time, end_time
		FROM segments WHERE id = ? AND session_id = ? AND transcript_revision = ?`,
		segmentID, sessionID, revision).Scan(
		&segment.Text, &segment.RecordingPath, &segment.RecordingSHA256, &segment.RecordingOrdinal,
		&segment.FirstWordOrdinal, &segment.LastWordOrdinal, &segment.Speaker, &segment.Start, &segment.End,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RefinedSegment{}, ErrSegmentNotFound
	}
	if err != nil {
		return RefinedSegment{}, fmt.Errorf("read refined segment %d: %w", segmentID, err)
	}
	if segment.RecordingPath == "" || segment.RecordingSHA256 == "" || segment.FirstWordOrdinal < 0 || segment.LastWordOrdinal < segment.FirstWordOrdinal {
		return RefinedSegment{}, ErrRefinedSegmentsUnavailable
	}
	return segment, nil
}

func replaceRefinedSegments(tx *sql.Tx, sessionID string, revision int, startedAt time.Time, words []RefinedWord) error {
	if _, err := tx.Exec(`DELETE FROM segments WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("clear refined segments: %w", err)
	}
	for _, segment := range BuildRefinedSegments(words) {
		timestamp := segment.Timestamp
		if timestamp.IsZero() {
			timestamp = startedAt.Add(time.Duration(segment.Start * float64(time.Second)))
		}
		if _, err := tx.Exec(`INSERT INTO segments(session_id, speaker, text, start_time, end_time, timestamp, transcript_revision, recording_path, recording_sha256, recording_ordinal, first_word_ordinal, last_word_ordinal)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, segment.Speaker, segment.Text, segment.Start, segment.End, timestamp.UTC().Format(time.RFC3339Nano), revision,
			segment.RecordingPath, segment.RecordingSHA256, segment.RecordingOrdinal, segment.FirstWordOrdinal, segment.LastWordOrdinal); err != nil {
			return fmt.Errorf("store rebuilt refined segment: %w", err)
		}
	}
	return nil
}

// moveRefinedWordsAfterSplice keeps the persisted occurrence coordinates in
// the spliced recording timeline and rebuilds their segments from those same
// occurrences.
func moveRefinedWordsAfterSplice(tx *sql.Tx, span *DeletedSpan) error {
	session, err := getSession(tx, span.SessionID)
	if err != nil {
		return err
	}
	if session.RefinedRevision == 0 {
		return nil
	}
	length := span.End - span.Start
	move := func(column string) string {
		return `CASE WHEN ` + column + ` <= ?1 THEN ` + column + ` WHEN ` + column + ` >= ?2 THEN ` + column + ` - ?3 ELSE ?1 END`
	}
	if _, err := tx.Exec(`UPDATE refined_words SET start_time = `+move("start_time")+`, end_time = `+move("end_time")+`, recording_sha256 = ?4
		WHERE session_id = ?5 AND transcript_revision = ?6 AND recording_sha256 = ?7
		  AND instr(recording_path, ?8) > 0`,
		span.Start, span.End, length, span.SHA256After, span.SessionID, session.RefinedRevision, span.SHA256Before, filepath.Base(span.AudioPath)); err != nil {
		return fmt.Errorf("move refined words after splice: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM refined_words
		WHERE session_id = ? AND transcript_revision = ? AND recording_sha256 = ? AND end_time <= start_time`,
		span.SessionID, session.RefinedRevision, span.SHA256After); err != nil {
		return fmt.Errorf("drop refined words cut from recording: %w", err)
	}
	words, err := refinedWords(tx, span.SessionID, session.RefinedRevision)
	if err != nil {
		return err
	}
	return replaceRefinedSegments(tx, span.SessionID, session.RefinedRevision, session.StartedAt, words)
}
