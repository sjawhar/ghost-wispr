package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

var (
	// ErrSegmentNotFound means the session does not contain the requested segment.
	ErrSegmentNotFound = errors.New("segment not found")
	// ErrSegmentSessionActive means a recording session cannot be changed yet.
	ErrSegmentSessionActive = errors.New("session is still active")
	// ErrSegmentPostCommitMaintenance means the deletion committed but the
	// write-ahead log checkpoint did not complete.
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
// carry its text ("Delete text and audio"). The database changes and the
// deleted span record commit in one transaction. File replacement follows the
// transaction; PendingSplices makes an interrupted replacement recoverable at
// startup.
//
//nolint:gocritic // The approved storage contract passes one DeletedSpan by value.
func (s *SQLiteStore) DeleteSegment(sessionID string, segmentID int64, span DeletedSpan) error {
	if span.SHA256After == "" {
		return fmt.Errorf("delete segment: invalid deleted span")
	}
	return s.deleteSegment(sessionID, segmentID, span, false)
}

// deleteSegment is both deletion actions. Each removes the segment and its
// words from every text store, on the session and every merged source, and
// erases them from the database files and the pre-migration backup. Only
// "Delete text and audio" (keepAudio false) records the splice of span's
// recording and owes Google Drive a replacement; "Delete text" (keepAudio
// true) records span, in the unchanged recording, for refinement to drop.
func (s *SQLiteStore) deleteSegment(sessionID string, segmentID int64, span DeletedSpan, keepAudio bool) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("delete segment: session ID is required")
	}
	if segmentID <= 0 {
		return fmt.Errorf("delete segment: segment ID is required")
	}
	if strings.TrimSpace(span.AudioPath) == "" || span.Start < 0 || span.End <= span.Start || span.SHA256Before == "" {
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
	all, err := getSegments(tx, sessionID)
	if err != nil {
		return fmt.Errorf("get segments for session %s: %w", sessionID, err)
	}
	var deleted transcribe.Segment
	remaining := make([]transcribe.Segment, 0, len(all))
	for _, segment := range all {
		if segment.ID == segmentID {
			deleted = segment
			continue
		}
		remaining = append(remaining, segment)
	}
	if _, err := tx.Exec(`DELETE FROM segments WHERE id = ? AND session_id = ?`, segmentID, sessionID); err != nil {
		return fmt.Errorf("delete segment %d from session %s: %w", segmentID, sessionID, err)
	}
	canonical := canonicalTranscript(remaining)
	// A session with no recording left to refine ("Delete text" of one whose
	// audio was removed to free space) has nothing a refinement could run on.
	if _, err := tx.Exec(`
		UPDATE sessions
		SET title = '',
			summary = '',
			summary_model = '',
			refined_transcript = '',
			refinement_model = '',
			refinement_status = CASE WHEN ? AND audio_path = '' THEN ? ELSE ? END,
			summary_status = ?,
			summary_error = '',
			canonical_transcript = ?,
			transcript_source = ?,
			summary_version = summary_version + 1,
			deletion_generation = deletion_generation + 1,
			envoy_publish_status = ?,
			sync_status = CASE WHEN sync_state = ? THEN sync_status ELSE ? END,
			sync_state = CASE WHEN sync_state = ? THEN sync_state ELSE ? END
		WHERE id = ?`,
		keepAudio, RefinementNoAudio, RefinementPending,
		SummaryPending,
		canonical,
		TranscriptSourceStreaming,
		EnvoyPublishPending,
		SyncStateSyncing, SyncPending,
		SyncStateSyncing, SyncStatePending,
		sessionID,
	); err != nil {
		return fmt.Errorf("clear deleted segment derivatives for session %s: %w", sessionID, err)
	}
	sources, holders, err := scrubMergedSources(tx, sessionID, remaining, deleted.Timestamp)
	if err != nil {
		return err
	}
	owners := append([]string{sessionID}, sources...)
	for _, owner := range owners {
		if _, err := tx.Exec(`DELETE FROM embeddings WHERE session_id = ?`, owner); err != nil {
			return fmt.Errorf("clear embeddings for session %s: %w", owner, err)
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO summary_requests(session_id, prompt_hash) VALUES(?, 'segment-delete')
		ON CONFLICT(session_id, prompt_hash) DO UPDATE SET created_at = CURRENT_TIMESTAMP`, sessionID); err != nil {
		return fmt.Errorf("queue summary for session %s: %w", sessionID, err)
	}
	if keepAudio {
		if err := recordTextDeletionSpan(tx, sessionID, sources, &span); err != nil {
			return err
		}
	} else if err := recordSplice(tx, sessionID, owners, &span, session.AudioPath); err != nil {
		return err
	}
	// The erase after the commit is owed until it finishes, so a failure, or
	// the app stopping, leaves it for RetryOwedErasure.
	owed, earlierOwed, err := recordOwedErase(tx)
	if err != nil {
		return err
	}
	if err := rewriteDeletedTextEvents(tx, owners, append([]string{sessionID}, holders...), deletedText, &span); err != nil {
		return fmt.Errorf("rewrite events for session %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild sessions search index: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete segment for session %s: %w", sessionID, err)
	}
	return s.eraseAfterDeletion(owed, earlierOwed, segmentID, deletedText, owners, append([]string{sessionID}, holders...), &span)
}

// recordSplice records, in tx, what only "Delete text and audio" does beyond
// the text side: the session names all its recordings again (span's
// SessionAudioPath, the ones restored from Drive included, else audioPath),
// owes Google Drive a replacement of every folder in owners that has one,
// with no earlier replacement's progress, and the span cut from its recording
// is recorded (deleted_segments) until its splice completes.
func recordSplice(tx *sql.Tx, sessionID string, owners []string, span *DeletedSpan, audioPath string) error {
	fullAudioPath := span.SessionAudioPath
	if fullAudioPath == "" {
		fullAudioPath = audioPath
	}
	if _, err := tx.Exec(`
		UPDATE sessions
		SET drive_replace_pending = 1,
			drive_replace_since = CASE WHEN drive_replace_pending = 1 AND drive_replace_since != '' THEN drive_replace_since ELSE ? END,
			drive_replace_failures = 0,
			drive_replace_error = '',
			audio_path = ?,
			audio_removed_at = ''
		WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano),
		fullAudioPath,
		sessionID,
	); err != nil {
		return fmt.Errorf("owe a Drive replacement for session %s: %w", sessionID, err)
	}
	if err := recordOwedDriveReplacementFolders(tx, sessionID, owners); err != nil {
		return err
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
		INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		sessionID, span.AudioPath, span.Start, span.End, span.SHA256Before, span.SHA256After, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("record deleted span for session %s: %w", sessionID, err)
	}
	return nil
}

// eraseAfterDeletion erases a committed deletion from the write-ahead log and
// the pre-migration backup, and clears the owed erase its transaction
// recorded (owed, its deletion number) unless a later deletion has recorded
// one since. When an earlier deletion's erase was owed too (earlierOwed), a scrub
// of the backup would take out only this deletion's words, so it runs the
// erase a retry runs, which covers both. A failure leaves the erase owed: the
// deletion's error wraps ErrSegmentPostCommitMaintenance, and
// RetryOwedErasure finishes it at the next start or sweep.
func (s *SQLiteStore) eraseAfterDeletion(owed string, earlierOwed bool, segmentID int64, deletedText string, owners, holders []string, span *DeletedSpan) error {
	s.eraseMu.Lock()
	defer s.eraseMu.Unlock()
	var err error
	if earlierOwed {
		err = s.eraseEveryOwed(false)
	} else {
		err = s.eraseDeletion(segmentID, deletedText, owners, holders, span)
	}
	if err == nil {
		err = clearOwedErase(s.db, owed)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSegmentPostCommitMaintenance, err)
	}
	return nil
}

// eraseDeletion erases one deletion's words from the write-ahead log and the
// pre-migration backup, and finishes any backup erase left part way.
func (s *SQLiteStore) eraseDeletion(segmentID int64, deletedText string, owners, holders []string, span *DeletedSpan) error {
	if err := postDeleteMaintenance(s.db, s.path+"-wal"); err != nil {
		return err
	}
	if err := s.eraseFromBackup(segmentID, deletedText, owners, holders, span); err != nil {
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	return s.eraseOrphans()
}

// postDeleteMaintenance moves a committed deletion out of the write-ahead log
// at walPath and empties the log, overwriting its older frames first: they
// still hold the deleted words, and truncating the log alone would leave them
// in blocks the file system frees. The database file itself needs nothing
// more. Every connection has secure_delete on (NewSQLiteStore), which zeroes
// the deleted content in the pages the deletion rewrites and every page it
// frees, the freelist included (SQLite's "fast" setting would leave the
// freelist; ON does not), so a VACUUM would only shrink the file: it rewrote
// the whole database through the log on every deletion.
var postDeleteMaintenance = func(db *sql.DB, walPath string) error {
	if err := overwriteWriteAheadLog(db, walPath); err != nil {
		return err
	}
	return truncateWriteAheadLog(db)
}

// scrubMergedSources removes a deleted segment's words from every session
// merged, directly or through another merge, into sessionID, and returns
// their ids (sources) and those of them that held the deleted segment
// (holders: the recorded session it came from, and each merged session
// between that one and sessionID). A merge moves its sources' segments to
// the merged session and leaves each source row (status merged) with the
// text it had: its title, summary, canonical and refined transcripts. A
// source's canonical transcript is rebuilt from the merged session's
// remaining segments that came from it; its title, summary and refined
// transcript, which cannot be rebuilt without the words, are cleared. The
// full-text index is rebuilt from these rows in the same transaction.
//
// A segment does not record the session it was recorded in, so it is
// assigned by time, without gaps: to the last recorded source (one nothing
// is merged into) that started at or before the segment, or to the first
// one if it started before every source. Sessions are recorded one at a
// time, so a segment cannot fall in another source's recording. One stamped
// after its session ended (a late final transcript) stays with it only until
// the next source started; one stamped later than that goes to that next
// source. A merged source holds every segment of the sources merged into it.
func scrubMergedSources(tx *sql.Tx, sessionID string, remaining []transcribe.Segment, deletedAt time.Time) (sources, holders []string, err error) {
	sources, err = mergedSourceIDs(tx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if len(sources) == 0 {
		return nil, nil, nil
	}
	parent := make(map[string]string, len(sources))
	hasChildren := make(map[string]bool, len(sources))
	var recorded []Session
	for _, id := range sources {
		source, err := getSession(tx, id)
		if err != nil {
			return nil, nil, fmt.Errorf("get merged source %s: %w", id, err)
		}
		parent[id] = source.MergedInto
		hasChildren[source.MergedInto] = true
		recorded = append(recorded, source)
	}
	var leaves []Session
	for i := range recorded {
		if !hasChildren[recorded[i].ID] {
			leaves = append(leaves, recorded[i])
		}
	}
	sort.SliceStable(leaves, func(i, j int) bool { return leaves[i].StartedAt.Before(leaves[j].StartedAt) })
	// chainOf is the recorded source a segment stamped at came from, and the
	// merged sources between it and sessionID.
	chainOf := func(at time.Time) []string {
		leaf := leaves[0].ID
		for i := range leaves {
			if !leaves[i].StartedAt.After(at) {
				leaf = leaves[i].ID
			}
		}
		var chain []string
		for id := leaf; id != "" && id != sessionID; id = parent[id] {
			chain = append(chain, id)
		}
		return chain
	}

	own := make(map[string][]transcribe.Segment, len(sources))
	for _, segment := range remaining {
		for _, id := range chainOf(segment.Timestamp) {
			own[id] = append(own[id], segment)
		}
	}
	for _, id := range sources {
		if _, err := tx.Exec(`UPDATE sessions
			SET title = '', summary = '', summary_model = '', refined_transcript = '', refinement_model = '', canonical_transcript = ?, transcript_source = ?
			WHERE id = ?`, canonicalTranscript(own[id]), TranscriptSourceStreaming, id); err != nil {
			return nil, nil, fmt.Errorf("scrub merged source %s: %w", id, err)
		}
	}
	return sources, chainOf(deletedAt), nil
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

// SourceSessionIDs returns the sessions merged, directly or through another
// merge, into mergedID, whose recordings (and any raw recording kept beside
// one) hold the merged session's audio, in session time order.
func (s *SQLiteStore) SourceSessionIDs(mergedID string) ([]string, error) {
	return mergedSourceIDs(s.db, mergedID)
}

// RecordingDirectories returns the directories, other than the audio
// directory, that recordings are kept or erased in: those of every absolute
// path in a session's audio_path or a deleted span's audio_path, and those a
// whole-file erase was started in (RecordEraseDirectory) until
// ForgetEraseDirectories forgets them. A relative path is read from the audio
// directory.
func (s *SQLiteStore) RecordingDirectories() ([]string, error) {
	rows, err := s.db.Query(`SELECT audio_path FROM sessions WHERE audio_path != ''
		UNION SELECT audio_path FROM deleted_segments WHERE audio_path != ''`)
	if err != nil {
		return nil, fmt.Errorf("list recording paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var dirs []string
	for rows.Next() {
		var paths string
		if err := rows.Scan(&paths); err != nil {
			return nil, fmt.Errorf("scan recording paths: %w", err)
		}
		for _, path := range strings.Split(paths, ",") {
			path = strings.TrimSpace(path)
			if !filepath.IsAbs(path) {
				continue
			}
			if dir := filepath.Dir(path); !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recording paths: %w", err)
	}
	erased, err := s.db.Query(`SELECT path FROM erase_directories`)
	if err != nil {
		return nil, fmt.Errorf("list erase directories: %w", err)
	}
	defer func() { _ = erased.Close() }()
	for erased.Next() {
		var dir string
		if err := erased.Scan(&dir); err != nil {
			return nil, fmt.Errorf("scan erase directory: %w", err)
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	if err := erased.Err(); err != nil {
		return nil, fmt.Errorf("iterate erase directories: %w", err)
	}
	return dirs, nil
}

// RecordEraseDirectory records dir, as the caller names it, before a
// whole-file erase in it begins (erase_directories), with the file system it
// is on. An erase cut off by a stop leaves its .erasing-* file there, and the
// row that named the file may be gone: the startup cleanup still reaches dir.
// A directory that is not there holds nothing to erase and is not recorded.
func (s *SQLiteStore) RecordEraseDirectory(dir string) error {
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat erase directory %s: %w", dir, err)
	}
	if _, err := s.db.Exec(`INSERT INTO erase_directories(path, device) VALUES(?, ?)
		ON CONFLICT(path) DO UPDATE SET device = excluded.device`, dir, fileSystemOf(info)); err != nil {
		return fmt.Errorf("record erase directory %s: %w", dir, err)
	}
	return nil
}

// ForgetEraseDirectories forgets the erase directories among dirs that the
// startup cleanup has finished every erase left in. One that is not there, or
// is on another file system than the erase was (a disk not mounted, its mount
// point showing the file system below), was not cleaned and is kept for a
// later start. Nothing else erases while the cleanup runs, so no erase
// recorded since is forgotten.
func (s *SQLiteStore) ForgetEraseDirectories(dirs []string) error {
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := s.db.Exec(`DELETE FROM erase_directories WHERE path = ? AND device = ?`, filepath.Clean(dir), fileSystemOf(info)); err != nil {
			return fmt.Errorf("forget erase directory %s: %w", dir, err)
		}
	}
	return nil
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

// rewriteDeletedTextEvents removes a deleted segment's words from stored
// events. Every session in sessionIDs (the one it was deleted from and those
// merged into it, whose summaries are cleared) loses its summary_ready events
// and every event with the deleted text. Only the sessions in holderIDs (the
// one it was deleted from and those that held the segment) lose the live
// transcript events that overlap the deleted span: the span is in the
// recording's own time, which every other session's live events share.
func rewriteDeletedTextEvents(tx *sql.Tx, sessionIDs, holderIDs []string, deletedText string, span *DeletedSpan) error {
	owners := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		owners[id] = true
	}
	holders := make(map[string]bool, len(holderIDs))
	for _, id := range holderIDs {
		holders[id] = true
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
		eventSessionID, _ := event["session_id"].(string)
		if !owners[eventSessionID] {
			continue
		}
		if isSummaryReadyEvent(eventType, event) || holders[eventSessionID] && isLiveTranscriptEvent(eventType, event) && eventOverlapsDeletedSpan(event, span) {
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
		if err := flushRecordingDirectory(span.AudioPath); err != nil {
			return nil, err
		}
		if err := s.MarkSpliceComplete(&span); err != nil {
			return nil, err
		}
	}
	return pending, nil
}

// flushRecordingDirectory flushes the directory holding recording, so the
// rename that put the spliced recording there is on disk before the splice is
// marked complete: after a power loss the mark must not stand beside the
// unspliced recording, which the Drive replacement would upload as the clean
// copy.
var flushRecordingDirectory = func(recording string) error {
	if err := syncFile(filepath.Dir(recording)); err != nil {
		return fmt.Errorf("flush recording directory of %s: %w", recording, err)
	}
	return nil
}

// MarkSpliceComplete lets garbage collection proceed after the recording has
// been atomically replaced with the span's expected post-splice bytes. In the
// same transaction, the first time it marks the span, it moves the spans
// "Delete text" recorded in the recording's pre-splice bytes to where their
// words are in the spliced one (moveTextDeletionSpans).
func (s *SQLiteStore) MarkSpliceComplete(span *DeletedSpan) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin marking deleted span complete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	match := `session_id = ? AND audio_path = ? AND start = ? AND end = ? AND sha256_before = ? AND sha256_after = ?`
	args := []any{span.SessionID, span.AudioPath, span.Start, span.End, span.SHA256Before, span.SHA256After}
	result, err := tx.Exec(`UPDATE deleted_segments SET splice_complete = 1 WHERE splice_complete = 0 AND `+match, args...)
	if err != nil {
		return fmt.Errorf("mark deleted span complete: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count completed deleted span: %w", err)
	}
	if rows == 0 {
		var marked int
		if err := tx.QueryRow(`SELECT count(*) FROM deleted_segments WHERE `+match, args...).Scan(&marked); err != nil {
			return fmt.Errorf("read deleted span: %w", err)
		}
		if marked == 0 {
			return fmt.Errorf("mark deleted span complete: %w", sql.ErrNoRows)
		}
		return nil
	}
	if err := moveTextDeletionSpans(tx, span); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit marking deleted span complete: %w", err)
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
