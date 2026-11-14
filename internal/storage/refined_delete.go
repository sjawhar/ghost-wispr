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
// transcription. An untimed segment comes from a merge source with no
// recording, so its revision is zero and Delete text removes its row directly.
func (s *SQLiteStore) DeleteRefinedSegmentText(sessionID string, segmentID int64, revision int) error {
	if revision == 0 {
		return s.deleteUntimedSegmentText(sessionID, segmentID)
	}
	return s.deleteRefinedSegment(sessionID, segmentID, revision, nil, true)
}

func (s *SQLiteStore) deleteUntimedSegmentText(sessionID string, segmentID int64) error {
	if segmentID <= 0 {
		return ErrSegmentNotFound
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin untimed segment deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	session, err := getSession(tx, sessionID)
	if err != nil {
		return err
	}
	if session.Status == SessionActive {
		return ErrSegmentSessionActive
	}
	var deletedText, untimedSourceID string
	if err := tx.QueryRow(`SELECT text, untimed_source_id FROM segments WHERE id = ? AND session_id = ? AND untimed = 1`, segmentID, sessionID).Scan(&deletedText, &untimedSourceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSegmentNotFound
		}
		return fmt.Errorf("read untimed segment %d: %w", segmentID, err)
	}
	if _, err := tx.Exec(`DELETE FROM segments WHERE id = ? AND session_id = ? AND untimed = 1`, segmentID, sessionID); err != nil {
		return fmt.Errorf("remove untimed segment %d: %w", segmentID, err)
	}

	nextRevision := session.RefinedRevision
	if nextRevision > 0 {
		words, err := refinedWords(tx, sessionID, session.RefinedRevision)
		if err != nil {
			return err
		}
		nextRevision++
		if _, err := tx.Exec(`UPDATE refined_words SET transcript_revision = ? WHERE session_id = ? AND transcript_revision = ?`, nextRevision, sessionID, session.RefinedRevision); err != nil {
			return fmt.Errorf("advance refined word revision after untimed deletion: %w", err)
		}
		if err := replaceRefinedSegments(tx, sessionID, nextRevision, session.StartedAt, words); err != nil {
			return err
		}
	}
	remaining, err := getSegments(tx, sessionID)
	if err != nil {
		return err
	}
	transcript := canonicalTranscript(remaining)
	source := TranscriptSourceStreaming
	if session.RefinementStatus == RefinementCompleted {
		source = TranscriptSourceRefined
	}
	if _, err := tx.Exec(`UPDATE sessions
		SET title = '', summary = '', summary_model = '', speaker_names = '{}',
			refined_transcript = ?, canonical_transcript = ?, transcript_source = ?,
			refinement_status = CASE WHEN refinement_status = ? THEN ? ELSE refinement_status END,
			summary_status = ?, summary_error = '', summary_version = summary_version + 1,
			deletion_generation = deletion_generation + 1,
			refinement_generation = deletion_generation + 1,
			refined_revision = ?,
			envoy_publish_status = ?,
			sync_status = CASE WHEN sync_state IN (?, ?) THEN sync_status ELSE ? END,
			sync_state = CASE WHEN sync_state IN (?, ?) THEN sync_state ELSE ? END
		WHERE id = ? AND refined_revision = ? AND status != ?`,
		transcript, transcript, source, RefinementRunning, RefinementFailed,
		SummaryPending, nextRevision, EnvoyPublishPending,
		SyncStateSyncing, SyncStateRemoteDeleted, SyncPending,
		SyncStateSyncing, SyncStateRemoteDeleted, SyncStatePending,
		sessionID, session.RefinedRevision, SessionMerged); err != nil {
		return fmt.Errorf("update untimed segment deletion session: %w", err)
	}
	sources, _, err := scrubMergedSourcesByRecording(tx, sessionID, "", untimedSourceID, deletedText, remaining)
	if err != nil {
		return err
	}
	if err := markDriveDocumentsPending(tx, sessionID, sources); err != nil {
		return err
	}
	if len(sources) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sources)), ",")
		args := make([]any, len(sources))
		for i, sourceID := range sources {
			args[i] = sourceID
		}
		if _, err := tx.Exec(`DELETE FROM refined_words WHERE session_id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("drop source refined words after untimed deletion: %w", err)
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
	owed, earlierOwed, err := recordOwedErase(tx)
	if err != nil {
		return err
	}
	if err := rewriteDeletedTextEvents(tx, owners, nil, deletedText, nil); err != nil {
		return fmt.Errorf("rewrite events for untimed deletion: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild search after untimed deletion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit untimed segment deletion: %w", err)
	}
	return s.eraseAfterDeletion(owed, earlierOwed, segmentID, deletedText, owners, nil, nil)
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
	transcript, err := transcriptWithUntimedSegments(tx, sessionID, RefinedTranscript(words))
	if err != nil {
		return err
	}
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
	sources, holders, err := scrubMergedSourcesByRecording(tx, sessionID, selected.RecordingPath, "", selected.Text, remaining)
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
		FROM segments WHERE id = ? AND session_id = ? AND transcript_revision = ? AND untimed = 0`,
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
	if _, err := tx.Exec(`DELETE FROM segments WHERE session_id = ? AND untimed = 0`, sessionID); err != nil {
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
	nextRevision := session.RefinedRevision + 1
	if _, err := tx.Exec(`UPDATE refined_words SET transcript_revision = ? WHERE session_id = ? AND transcript_revision = ?`,
		nextRevision, span.SessionID, session.RefinedRevision); err != nil {
		return fmt.Errorf("advance refined word revision after splice: %w", err)
	}
	length := span.End - span.Start
	move := func(column string) string {
		return `CASE WHEN ` + column + ` <= ?1 THEN ` + column + ` WHEN ` + column + ` >= ?2 THEN ` + column + ` - ?3 ELSE ?1 END`
	}
	recordingBase := filepath.Base(span.AudioPath)
	if _, err := tx.Exec(`UPDATE refined_words SET start_time = `+move("start_time")+`, end_time = `+move("end_time")+`, recording_sha256 = ?4
		WHERE session_id = ?5 AND transcript_revision = ?6 AND recording_sha256 = ?7
		  AND (recording_path = ?8 OR recording_path LIKE ?9 OR recording_path = ?10)`,
		span.Start, span.End, length, span.SHA256After, span.SessionID, nextRevision, span.SHA256Before,
		span.AudioPath, "%/"+recordingBase, recordingBase); err != nil {
		return fmt.Errorf("move refined words after splice: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM refined_words
		WHERE session_id = ? AND transcript_revision = ? AND recording_sha256 = ? AND end_time <= start_time`,
		span.SessionID, nextRevision, span.SHA256After); err != nil {
		return fmt.Errorf("drop refined words cut from recording: %w", err)
	}
	words, err := refinedWords(tx, span.SessionID, nextRevision)
	if err != nil {
		return err
	}
	if err := replaceRefinedSegments(tx, span.SessionID, nextRevision, session.StartedAt, words); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sessions SET refined_revision = ? WHERE id = ? AND refined_revision = ?`,
		nextRevision, span.SessionID, session.RefinedRevision); err != nil {
		return fmt.Errorf("advance refined session revision after splice: %w", err)
	}
	return nil
}
