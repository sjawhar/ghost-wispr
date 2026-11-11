package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

var (
	// ErrSegmentNotFound means the session does not contain the requested segment.
	ErrSegmentNotFound = errors.New("segment not found")
	// ErrSegmentSessionActive means a recording session cannot be changed yet.
	ErrSegmentSessionActive = errors.New("session is still active")
	// ErrSegmentPostCommitMaintenance means the deletion committed but SQLite
	// maintenance did not complete.
	ErrSegmentPostCommitMaintenance = errors.New("segment deletion post-commit maintenance failed")
	// ErrSegmentSplicePending means an earlier deletion still needs recovery.
	ErrSegmentSplicePending = errors.New("segment splice is pending")
)

// DeletedSpan identifies the session recording span removed for a segment and
// the recording hashes on each side of the splice.
type DeletedSpan struct {
	SessionID                 string
	AudioPath                 string
	SessionAudioPath          string
	Start, End                float64
	StreamStart, StreamEnd    float64
	SHA256Before, SHA256After string
}

// DeleteSegment removes one saved segment and clears every derivative that can
// carry its text. The database changes and the deleted span record commit in
// one transaction. File replacement follows the transaction; PendingSplices
// makes an interrupted replacement recoverable at startup.
//
//nolint:gocritic // The approved storage contract passes one DeletedSpan by value.
func (s *SQLiteStore) DeleteSegment(sessionID string, segmentID int64, span DeletedSpan) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("delete segment: session ID is required")
	}
	if segmentID <= 0 {
		return fmt.Errorf("delete segment: segment ID is required")
	}
	if strings.TrimSpace(span.AudioPath) == "" || span.Start < 0 || span.End <= span.Start || span.SHA256Before == "" || span.SHA256After == "" {
		return fmt.Errorf("delete segment: invalid deleted span")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin delete segment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	session, err := getSession(tx, sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if session.Status == SessionActive {
		return ErrSegmentSessionActive
	}
	fullAudioPath := span.SessionAudioPath
	if fullAudioPath == "" {
		fullAudioPath = session.AudioPath
	}

	var deletedText string
	var segmentStart, segmentEnd float64
	if err := tx.QueryRow(`SELECT text, start_time, end_time FROM segments WHERE id = ? AND session_id = ?`, segmentID, sessionID).Scan(&deletedText, &segmentStart, &segmentEnd); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSegmentNotFound
		}
		return fmt.Errorf("get segment %d from session %s: %w", segmentID, sessionID, err)
	}
	if span.StreamEnd <= span.StreamStart {
		span.StreamStart, span.StreamEnd = segmentStart, segmentEnd
	}
	if _, err := tx.Exec(`DELETE FROM segments WHERE id = ? AND session_id = ?`, segmentID, sessionID); err != nil {
		return fmt.Errorf("delete segment %d from session %s: %w", segmentID, sessionID, err)
	}

	remaining, err := getSegments(tx, sessionID)
	if err != nil {
		return fmt.Errorf("get remaining segments for session %s: %w", sessionID, err)
	}
	canonical := canonicalTranscript(remaining)
	if _, err := tx.Exec(`
		UPDATE sessions
		SET title = '',
			summary = '',
			refined_transcript = '',
			refinement_status = ?,
			summary_status = ?,
			summary_error = '',
			canonical_transcript = ?,
			transcript_source = ?,
			summary_version = summary_version + 1,
			deletion_generation = deletion_generation + 1,
			envoy_publish_status = ?,
			sync_status = ?,
			sync_state = ?,
			drive_replace_pending = 1,
			audio_path = ?,
			audio_removed_at = ''
		WHERE id = ?`,
		RefinementPending,
		SummaryPending,
		canonical,
		TranscriptSourceStreaming,
		EnvoyPublishPending,
		SyncPending,
		SyncStatePending,
		fullAudioPath,
		sessionID,
	); err != nil {
		return fmt.Errorf("clear deleted segment derivatives for session %s: %w", sessionID, err)
	}
	sources, err := scrubMergedSources(tx, sessionID, remaining)
	if err != nil {
		return err
	}
	owners := append([]string{sessionID}, sources...)
	for _, owner := range owners {
		if _, err := tx.Exec(`DELETE FROM embeddings WHERE session_id = ?`, owner); err != nil {
			return fmt.Errorf("clear embeddings for session %s: %w", owner, err)
		}
	}
	if exists, err := tableExists(tx, "drive_replacements"); err != nil {
		return fmt.Errorf("check prior Drive replacements for session %s: %w", sessionID, err)
	} else if exists {
		for _, owner := range owners {
			if _, err := tx.Exec(`DELETE FROM drive_replacements WHERE session_id = ?`, owner); err != nil {
				return fmt.Errorf("clear prior Drive replacements for session %s: %w", owner, err)
			}
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO summary_requests(session_id, prompt_hash) VALUES(?, 'segment-delete')
		ON CONFLICT(session_id, prompt_hash) DO UPDATE SET created_at = CURRENT_TIMESTAMP`, sessionID); err != nil {
		return fmt.Errorf("queue summary for session %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(`
		INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		sessionID, span.AudioPath, span.Start, span.End, span.SHA256Before, span.SHA256After, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("record deleted span for session %s: %w", sessionID, err)
	}
	if err := rewriteDeletedTextEvents(tx, owners, deletedText, &span); err != nil {
		return fmt.Errorf("rewrite events for session %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild sessions search index: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete segment for session %s: %w", sessionID, err)
	}
	if err := postDeleteMaintenance(s.db); err != nil {
		return fmt.Errorf("%w: %v", ErrSegmentPostCommitMaintenance, err)
	}
	return nil
}

var postDeleteMaintenance = func(db *sql.DB) error {
	if _, err := db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

// scrubMergedSources removes a deleted segment's words from every session
// merged, directly or through another merge, into sessionID, and returns
// their ids. A merge moves its sources' segments to the merged session and
// leaves each source row (status merged) with the text it had: its title,
// summary, canonical and refined transcripts, all of which can hold the
// deleted words. A source's canonical transcript is rebuilt from the merged
// session's remaining segments in the source's time span, which are its own
// segments less the deleted one; its title, summary and refined transcript,
// which cannot be rebuilt without the words, are cleared. The full-text
// index is rebuilt from these rows in the same transaction.
func scrubMergedSources(tx *sql.Tx, sessionID string, remaining []transcribe.Segment) ([]string, error) {
	sources, err := mergedSourceIDs(tx, sessionID)
	if err != nil {
		return nil, err
	}

	for _, id := range sources {
		source, err := getSession(tx, id)
		if err != nil {
			return nil, fmt.Errorf("get merged source %s: %w", id, err)
		}
		var own []transcribe.Segment
		for _, segment := range remaining {
			if !segment.Timestamp.Before(source.StartedAt) && (source.EndedAt == nil || !segment.Timestamp.After(*source.EndedAt)) {
				own = append(own, segment)
			}
		}
		if _, err := tx.Exec(`UPDATE sessions
			SET title = '', summary = '', refined_transcript = '', canonical_transcript = ?, transcript_source = ?
			WHERE id = ?`, canonicalTranscript(own), TranscriptSourceStreaming, id); err != nil {
			return nil, fmt.Errorf("scrub merged source %s: %w", id, err)
		}
	}
	return sources, nil
}

func canonicalTranscript(segments []transcribe.Segment) string {
	var builder strings.Builder
	for _, segment := range segments {
		text := strings.TrimSpace(segment.Text)
		if text == "" {
			continue
		}
		builder.WriteString(text)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func tableExists(tx *sql.Tx, name string) (bool, error) {
	var found int
	err := tx.QueryRow(`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// SourceSessionIDs returns the source sessions whose recordings a merged
// session owns, in session time order.
func (s *SQLiteStore) SourceSessionIDs(mergedID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE merged_into = ? ORDER BY started_at ASC`, mergedID)
	if err != nil {
		return nil, fmt.Errorf("query merged sources for %s: %w", mergedID, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan merged source: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate merged sources for %s: %w", mergedID, err)
	}
	return ids, nil
}

// mergedSourceIDs returns the sessions merged, directly or through another
// merge, into sessionID, in session time order.
func mergedSourceIDs(q querier, sessionID string) ([]string, error) {
	rows, err := q.Query(`WITH RECURSIVE source(id) AS (
			SELECT id FROM sessions WHERE merged_into = ?
			UNION SELECT sessions.id FROM sessions JOIN source ON sessions.merged_into = source.id
		) SELECT sessions.id FROM sessions JOIN source ON sessions.id = source.id ORDER BY sessions.started_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list merged sources of session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()
	var sources []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan merged source of session %s: %w", sessionID, err)
		}
		sources = append(sources, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate merged sources of session %s: %w", sessionID, err)
	}
	return sources, nil
}

// MergedSources returns the sessions merged, directly or through another
// merge, into sessionID. Each keeps its own Drive folder, which a deletion in
// sessionID must also reach.
func (s *SQLiteStore) MergedSources(sessionID string) ([]Session, error) {
	ids, err := mergedSourceIDs(s.db, sessionID)
	if err != nil {
		return nil, err
	}
	sources := make([]Session, 0, len(ids))
	for _, id := range ids {
		source, err := getSession(s.db, id)
		if err != nil {
			return nil, fmt.Errorf("get merged source %s: %w", id, err)
		}
		sources = append(sources, source)
	}
	return sources, nil
}

// HasPendingSplice reports whether a committed deletion for sessionID still
// requires its recording replacement to complete.
func (s *SQLiteStore) HasPendingSplice(sessionID string) (bool, error) {
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM deleted_segments WHERE session_id = ? AND splice_complete = 0`, sessionID).Scan(&count); err != nil {
		return false, fmt.Errorf("query pending splice for %s: %w", sessionID, err)
	}
	return count > 0, nil
}

// OldestPendingSplice reports when the oldest committed deletion for sessionID
// started waiting for its recording splice.
func (s *SQLiteStore) OldestPendingSplice(sessionID string) (time.Time, bool, error) {
	var deletedAt string
	err := s.db.QueryRow(`SELECT deleted_at FROM deleted_segments
		WHERE session_id = ? AND splice_complete = 0
		ORDER BY deleted_at ASC, rowid ASC LIMIT 1`, sessionID).Scan(&deletedAt)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read oldest pending splice for %s: %w", sessionID, err)
	}
	at, err := time.Parse(time.RFC3339Nano, deletedAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse oldest pending splice for %s: %w", sessionID, err)
	}
	return at, true, nil
}

// rewriteDeletedTextEvents removes a deleted segment's words from the stored
// events of sessionIDs: the session it was deleted from and the sessions
// merged into it, whose live events recorded the words first.
func rewriteDeletedTextEvents(tx *sql.Tx, sessionIDs []string, deletedText string, span *DeletedSpan) error {
	owners := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		owners[id] = true
	}
	rows, err := tx.Query(`SELECT id, event_type, payload FROM mcp_events`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	type update struct {
		id      int64
		payload string
		delete  bool
	}
	var updates []update
	for rows.Next() {
		var id int64
		var eventType, payload string
		if err := rows.Scan(&id, &eventType, &payload); err != nil {
			return err
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if eventSessionID, _ := event["session_id"].(string); !owners[eventSessionID] {
			continue
		}
		if isSummaryReadyEvent(eventType, event) || isLiveTranscriptEvent(eventType, event) && eventOverlapsDeletedSpan(event, span) {
			updates = append(updates, update{id: id, delete: true})
			continue
		}
		redacted, changed := redactEventText(event, deletedText)
		if !changed {
			continue
		}
		rewritten, err := json.Marshal(redacted)
		if err != nil {
			return err
		}
		updates = append(updates, update{id: id, payload: string(rewritten)})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if update.delete {
			if _, err := tx.Exec(`DELETE FROM mcp_events WHERE id = ?`, update.id); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`UPDATE mcp_events SET payload = ? WHERE id = ?`, update.payload, update.id); err != nil {
			return err
		}
	}
	return nil
}

func isLiveTranscriptEvent(eventType string, event map[string]any) bool {
	if eventType == "live_transcript" || eventType == "live_transcript_interim" {
		return true
	}
	typeName, _ := event["type"].(string)
	return typeName == "live_transcript" || typeName == "live_transcript_interim"
}

func isSummaryReadyEvent(eventType string, event map[string]any) bool {
	if eventType == "summary_ready" {
		return true
	}
	typeName, _ := event["type"].(string)
	return typeName == "summary_ready"
}

func eventOverlapsDeletedSpan(event map[string]any, span *DeletedSpan) bool {
	start, ok := event["start_time"].(float64)
	if !ok {
		return false
	}
	end, hasEnd := event["end_time"].(float64)
	if !hasEnd {
		return start >= span.StreamStart && start < span.StreamEnd
	}
	return start < span.StreamEnd && end > span.StreamStart
}

func redactEventText(value any, deletedText string) (any, bool) {
	switch typed := value.(type) {
	case string:
		redacted := strings.ReplaceAll(typed, deletedText, "")
		return redacted, redacted != typed
	case []any:
		changed := false
		copy := make([]any, len(typed))
		for i, entry := range typed {
			redacted, entryChanged := redactEventText(entry, deletedText)
			copy[i] = redacted
			changed = changed || entryChanged
		}
		return copy, changed
	case map[string]any:
		changed := false
		copy := make(map[string]any, len(typed))
		for key, entry := range typed {
			redacted, entryChanged := redactEventText(entry, deletedText)
			copy[key] = redacted
			changed = changed || entryChanged
		}
		return copy, changed
	default:
		return value, false
	}
}

// PendingSplices returns deletions whose recording still has the pre-splice
// hash, plus rows whose recording is absent and must be restored first. A row
// with the recorded post-splice hash is completed before this returns.
func (s *SQLiteStore) PendingSplices() ([]DeletedSpan, error) {
	rows, err := s.db.Query(`SELECT session_id, audio_path, start, end, sha256_before, sha256_after FROM deleted_segments WHERE splice_complete = 0 ORDER BY deleted_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query deleted spans: %w", err)
	}

	var pending, complete []DeletedSpan
	for rows.Next() {
		var span DeletedSpan
		if err := rows.Scan(&span.SessionID, &span.AudioPath, &span.Start, &span.End, &span.SHA256Before, &span.SHA256After); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan deleted span: %w", err)
		}
		hash, err := sha256ForFile(span.AudioPath)
		switch {
		case errors.Is(err, os.ErrNotExist), hash == span.SHA256Before:
			pending = append(pending, span)
		case err != nil:
			_ = rows.Close()
			return nil, fmt.Errorf("hash deleted recording %s: %w", span.AudioPath, err)
		case hash == span.SHA256After:
			complete = append(complete, span)
		default:
			pending = append(pending, span)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate deleted spans: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close deleted spans: %w", err)
	}
	for _, span := range complete {
		if err := s.MarkSpliceComplete(&span); err != nil {
			return nil, err
		}
	}
	return pending, nil
}

// MarkSpliceComplete lets garbage collection proceed after the recording has
// been atomically replaced with the span's expected post-splice bytes.
func (s *SQLiteStore) MarkSpliceComplete(span *DeletedSpan) error {
	result, err := s.db.Exec(`
		UPDATE deleted_segments SET splice_complete = 1
		WHERE session_id = ? AND audio_path = ? AND start = ? AND end = ?
		  AND sha256_before = ? AND sha256_after = ?`,
		span.SessionID, span.AudioPath, span.Start, span.End, span.SHA256Before, span.SHA256After,
	)
	if err != nil {
		return fmt.Errorf("mark deleted span complete: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count completed deleted span: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("mark deleted span complete: %w", sql.ErrNoRows)
	}
	return nil
}

func sha256ForFile(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}
