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
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/recording"
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
	// ErrStaleTranscriptRevision means a browser selected a segment before a
	// later refinement or deletion rebuilt the timed segment table.
	ErrStaleTranscriptRevision = errors.New("stale transcript revision")
	// ErrRefinedSegmentsUnavailable means a session needs batch refinement
	// before an occurrence-based deletion can be made.
	ErrRefinedSegmentsUnavailable = errors.New("refined segments are unavailable")
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

func scrubMergedSourcesByRecording(tx *sql.Tx, sessionID, deletedRecording string, remaining []transcribe.Segment) (sources, holders []string, err error) {
	sources, err = mergedSourceIDs(tx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if len(sources) == 0 {
		return nil, nil, nil
	}
	parents := make(map[string]string, len(sources))
	for _, source := range sources {
		session, err := getSession(tx, source)
		if err != nil {
			return nil, nil, fmt.Errorf("get merged source %s: %w", source, err)
		}
		parents[source] = session.MergedInto
	}

	// Merges made before recording_sources existed still carry recording paths
	// whose base name is their source session id. Backfill those direct
	// ownership rows before assigning retained words to source transcripts.
	sourceForRecording := func(recording string) string {
		base := strings.TrimSuffix(filepath.Base(recording), filepath.Ext(recording))
		base = strings.TrimPrefix(base, "recording-")
		base = strings.TrimPrefix(base, "audio-")
		for _, source := range sources {
			if source == base {
				return source
			}
		}
		return ""
	}
	recordings := []string{deletedRecording}
	for i := range remaining {
		recordings = append(recordings, remaining[i].RecordingPath)
	}
	for _, recording := range recordings {
		source := sourceForRecording(recording)
		if source == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO recording_sources(merged_session_id, recording_path, source_session_id) VALUES(?, ?, ?)`, sessionID, recording, source); err != nil {
			return nil, nil, fmt.Errorf("backfill merged recording source %s: %w", recording, err)
		}
	}

	rows, err := tx.Query(`WITH RECURSIVE merged(id) AS (
			SELECT ? UNION SELECT sessions.id FROM sessions JOIN merged ON sessions.merged_into = merged.id
		) SELECT merged_session_id, recording_path, source_session_id
		FROM recording_sources WHERE merged_session_id IN (SELECT id FROM merged)`, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("read merged recording sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owners := make(map[string]map[string]string)
	for rows.Next() {
		var merged, recording, source string
		if err := rows.Scan(&merged, &recording, &source); err != nil {
			return nil, nil, fmt.Errorf("scan merged recording source: %w", err)
		}
		if owners[merged] == nil {
			owners[merged] = make(map[string]string)
		}
		owners[merged][recording] = source
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate merged recording sources: %w", err)
	}

	recordingChain := func(recording string) []string {
		owner := owners[sessionID][recording]
		seen := make(map[string]struct{})
		for owner != "" {
			if _, duplicate := seen[owner]; duplicate {
				return nil
			}
			seen[owner] = struct{}{}
			if nested := owners[owner][recording]; nested != "" {
				owner = nested
				continue
			}
			break
		}
		var chain []string
		for owner != "" && owner != sessionID {
			chain = append(chain, owner)
			owner = parents[owner]
		}
		return chain
	}

	own := make(map[string][]transcribe.Segment, len(sources))
	for i := range remaining {
		for _, source := range recordingChain(remaining[i].RecordingPath) {
			own[source] = append(own[source], remaining[i])
		}
	}
	holders = recordingChain(deletedRecording)
	mappedRecording := len(holders) != 0
	for _, source := range sources {
		if mappedRecording && len(own[source]) == 0 && !slices.Contains(holders, source) {
			continue
		}
		if _, err := tx.Exec(`UPDATE sessions
			SET title = '', summary = '', summary_model = '', speaker_names = '{}',
			    refined_transcript = '', refinement_model = '',
			    canonical_transcript = ?, transcript_source = ?, deletion_generation = deletion_generation + 1
			WHERE id = ?`, canonicalTranscript(own[source]), TranscriptSourceStreaming, source); err != nil {
			return nil, nil, fmt.Errorf("scrub merged source %s by recording: %w", source, err)
		}
	}
	return sources, holders, nil
}

func canonicalTranscript(segments []transcribe.Segment) string {
	var builder strings.Builder
	for i := range segments {
		text := strings.TrimSpace(segments[i].Text)
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

// RecordingDirectories returns the directories, besides the audio directory,
// that recordings are kept or erased in: that of every path in a session's
// audio_path or a deleted span's audio_path, where the path points (a
// relative one from the working directory, where the recorder writes
// filepath.Join(audio_dir, name) and recording.Resolve reads it first), as an
// absolute path, and those a whole-file erase was started in
// (RecordEraseDirectory) until ForgetEraseDirectories forgets them.
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
			if path = strings.TrimSpace(path); path == "" {
				continue
			}
			dir, err := filepath.Abs(filepath.Dir(path))
			if err != nil {
				return nil, fmt.Errorf("find the directory of recording %s: %w", path, err)
			}
			if !slices.Contains(dirs, dir) {
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

// RecordEraseDirectory records dir, as an absolute path, before a whole-file
// erase in it begins (erase_directories), with the file system it is on. An
// erase cut off by a stop leaves its .erasing-* file there, and the row that
// named the file may be gone: the startup cleanup still reaches dir. A
// directory that is not there holds nothing to erase and is not recorded.
func (s *SQLiteStore) RecordEraseDirectory(dir string) error {
	return recordEraseDirectory(s.db, dir)
}

// execer is what writing a row needs: the database, or a transaction.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// recordEraseDirectory is RecordEraseDirectory in q: DeleteSession records
// in the transaction that deletes the row.
func recordEraseDirectory(q execer, dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("find the erase directory %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat erase directory %s: %w", dir, err)
	}
	if _, err := q.Exec(`INSERT OR IGNORE INTO erase_directories(path, device) VALUES(?, ?)`, dir, fileSystemOf(info)); err != nil {
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
		dir, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("find the erase directory %s: %w", dir, err)
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := s.db.Exec(`DELETE FROM erase_directories WHERE path = ? AND device = ?`, dir, fileSystemOf(info)); err != nil {
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

// DeletionsToRefine returns the sessions whose segment deletion has its
// recording spliced (none of its splices is pending), or that "Delete text"
// changed, but whose refinement or summary has not run since: refinement
// still pending, as the deletion set it, or a summary pending or cut off
// running. The deletion runner refines and summarizes the session after its
// splice, or after "Delete text", so a stop between the two leaves such a
// session, and nothing else starts either. The Drive replacement waits for
// both, so the startup recovery runs them. A merged session's sources, an
// active session, and one with no audio to refine (the startup summary pass
// makes its summary) are not listed.
func (s *SQLiteStore) DeletionsToRefine() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions
		WHERE status NOT IN (?, ?)
		  AND (EXISTS (SELECT 1 FROM deleted_segments WHERE deleted_segments.session_id = sessions.id)
		       OR EXISTS (SELECT 1 FROM deleted_text_spans WHERE deleted_text_spans.session_id = sessions.id))
		  AND NOT EXISTS (SELECT 1 FROM deleted_segments WHERE deleted_segments.session_id = sessions.id AND splice_complete = 0)
		  AND audio_path != '' AND refinement_status != ?
		  AND (refinement_status = ? OR summary_status IN (?, ?))
		ORDER BY started_at ASC`,
		SessionActive, SessionMerged, RefinementNoAudio, RefinementPending, SummaryPending, SummaryRunning)
	if err != nil {
		return nil, fmt.Errorf("list segment deletions to refine: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan segment deletion to refine: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate segment deletions to refine: %w", err)
	}
	return ids, nil
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

// dropLiveTranscriptEvents removes live transcript events for sessions whose
// segment table was rebuilt. Their socket-relative times cannot safely map to
// a batch recording occurrence.
func dropLiveTranscriptEvents(tx *sql.Tx, sessionIDs []string) error {
	seen := make(map[string]struct{}, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		if sessionID == "" {
			continue
		}
		if _, duplicate := seen[sessionID]; duplicate {
			continue
		}
		seen[sessionID] = struct{}{}
		if _, err := tx.Exec(`DELETE FROM mcp_events
			WHERE (event_type IN ('live_transcript', 'live_transcript_interim')
				OR (json_valid(payload) AND COALESCE(json_extract(payload, '$.type'), '') IN ('live_transcript', 'live_transcript_interim')))
			  AND (NOT json_valid(payload) OR COALESCE(json_extract(payload, '$.session_id'), '') = ?)`, sessionID); err != nil {
			return fmt.Errorf("drop live transcript events for session %s: %w", sessionID, err)
		}
	}
	return nil
}

// PendingSplices returns deletions whose recording still has the pre-splice
// hash, plus rows whose recording is absent and must be restored first. It
// finds each recording where recording.Resolve finds it in audioDir. A row
// with the recorded post-splice hash is completed before this returns.
func (s *SQLiteStore) PendingSplices(audioDir string) ([]DeletedSpan, error) {
	rows, err := s.db.Query(`SELECT session_id, audio_path, start, end, sha256_before, sha256_after FROM deleted_segments WHERE splice_complete = 0 ORDER BY deleted_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query deleted spans: %w", err)
	}

	type completedSplice struct {
		span      DeletedSpan
		recording string
	}
	var pending []DeletedSpan
	var complete []completedSplice
	for rows.Next() {
		var span DeletedSpan
		if err := rows.Scan(&span.SessionID, &span.AudioPath, &span.Start, &span.End, &span.SHA256Before, &span.SHA256After); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan deleted span: %w", err)
		}
		path := recording.Resolve(span.AudioPath, audioDir)
		hash, err := sha256ForFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist), hash == span.SHA256Before:
			pending = append(pending, span)
		case err != nil:
			_ = rows.Close()
			return nil, fmt.Errorf("hash deleted recording %s: %w", path, err)
		case hash == span.SHA256After:
			complete = append(complete, completedSplice{span: span, recording: path})
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
	for i := range complete {
		if err := flushRecordingDirectory(complete[i].recording); err != nil {
			return nil, err
		}
		if err := s.MarkSpliceComplete(&complete[i].span); err != nil {
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
// words are in the spliced one (moveTextDeletionSpans), and it clears the
// session's pending Drive replacement when the deletion owes Drive nothing
// (settleUnownedDriveReplacement).
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
	} else {
		if err := moveTextDeletionSpans(tx, span); err != nil {
			return err
		}
		if err := moveRefinedWordsAfterSplice(tx, span); err != nil {
			return err
		}
	}
	if err := settleUnownedDriveReplacement(tx, span.SessionID); err != nil {
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
