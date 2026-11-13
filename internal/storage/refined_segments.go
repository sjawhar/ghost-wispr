package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// RefinedWord is one batch-transcribed occurrence. RecordingPath and
// RecordingSHA256 identify the exact recording revision that supplied it.
type RefinedWord struct {
	RecordingPath    string
	RecordingSHA256  string
	RecordingOrdinal int
	Ordinal          int
	Text             string
	Speaker          int
	Start, End       float64
}

// RefinedSegment groups consecutive RefinedWords from one recording. The word
// ordinals make its deletion span independent of the displayed text.
type RefinedSegment struct {
	RecordingPath    string
	RecordingSHA256  string
	RecordingOrdinal int
	FirstWordOrdinal int
	LastWordOrdinal  int
	Speaker          int
	Text             string
	Start, End       float64
	Timestamp        time.Time
}

// PublishRefinementAtGeneration replaces live segments with a completed batch
// refinement in one transaction. The generation fence rejects a result that
// started before a deletion changed the session.
func (s *SQLiteStore) PublishRefinementAtGeneration(sessionID, transcript, model string, words []RefinedWord, segments []RefinedSegment, generation int) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin publish refinement for session %s: %w", sessionID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var deletionGeneration, refinementGeneration, revision int
	var startedAt string
	if err := tx.QueryRow(`SELECT deletion_generation, refinement_generation, refined_revision, started_at FROM sessions WHERE id = ?`, sessionID).Scan(&deletionGeneration, &refinementGeneration, &revision, &startedAt); err != nil {
		return 0, fmt.Errorf("read refinement generation for session %s: %w", sessionID, err)
	}
	if deletionGeneration != generation || refinementGeneration != generation {
		return 0, sql.ErrNoRows
	}
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return 0, fmt.Errorf("parse session %s start: %w", sessionID, err)
	}
	nextRevision := revision + 1

	if _, err := tx.Exec(`DELETE FROM refined_words WHERE session_id = ?`, sessionID); err != nil {
		return 0, fmt.Errorf("clear refined words for session %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(`DELETE FROM segments WHERE session_id = ?`, sessionID); err != nil {
		return 0, fmt.Errorf("clear live segments for session %s: %w", sessionID, err)
	}
	for _, word := range words {
		if strings.TrimSpace(word.RecordingPath) == "" || strings.TrimSpace(word.RecordingSHA256) == "" || word.Ordinal < 0 || word.End < word.Start {
			return 0, fmt.Errorf("publish refinement for session %s: invalid word provenance", sessionID)
		}
		if _, err := tx.Exec(`INSERT INTO refined_words(session_id, transcript_revision, recording_path, recording_sha256, recording_ordinal, ordinal, text, speaker, start_time, end_time)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, nextRevision, word.RecordingPath, word.RecordingSHA256, word.RecordingOrdinal, word.Ordinal, strings.TrimSpace(word.Text), word.Speaker, word.Start, word.End); err != nil {
			return 0, fmt.Errorf("store refined word for session %s: %w", sessionID, err)
		}
	}
	for _, segment := range segments {
		if strings.TrimSpace(segment.RecordingPath) == "" || strings.TrimSpace(segment.RecordingSHA256) == "" || segment.FirstWordOrdinal < 0 || segment.LastWordOrdinal < segment.FirstWordOrdinal || segment.End < segment.Start {
			return 0, fmt.Errorf("publish refinement for session %s: invalid segment provenance", sessionID)
		}
		timestamp := segment.Timestamp
		if timestamp.IsZero() {
			timestamp = started.Add(time.Duration(segment.Start * float64(time.Second)))
		}
		if _, err := tx.Exec(`INSERT INTO segments(session_id, speaker, text, start_time, end_time, timestamp, transcript_revision, recording_path, recording_sha256, recording_ordinal, first_word_ordinal, last_word_ordinal)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, segment.Speaker, strings.TrimSpace(segment.Text), segment.Start, segment.End, timestamp.UTC().Format(time.RFC3339Nano), nextRevision,
			segment.RecordingPath, segment.RecordingSHA256, segment.RecordingOrdinal, segment.FirstWordOrdinal, segment.LastWordOrdinal); err != nil {
			return 0, fmt.Errorf("store refined segment for session %s: %w", sessionID, err)
		}
	}

	canonical := strings.TrimSpace(transcript)
	res, err := tx.Exec(`UPDATE sessions
		SET refined_transcript = ?, refinement_status = ?, refinement_model = ?,
			canonical_transcript = ?, transcript_source = ?, refined_revision = ?
		WHERE id = ? AND deletion_generation = ? AND refinement_generation = ?`,
		canonical, RefinementCompleted, model, canonical, TranscriptSourceRefined, nextRevision, sessionID, generation, generation)
	if err != nil {
		return 0, fmt.Errorf("publish refinement for session %s: %w", sessionID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read refinement publication for session %s: %w", sessionID, err)
	}
	if rows == 0 {
		return 0, sql.ErrNoRows
	}
	if _, err := tx.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')`); err != nil {
		return 0, fmt.Errorf("rebuild search after refining session %s: %w", sessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit refinement for session %s: %w", sessionID, err)
	}
	return nextRevision, nil
}

// RefinedWords returns the current persisted occurrences for a transcript
// revision in recording and ordinal order.
func (s *SQLiteStore) RefinedWords(sessionID string, revision int) ([]RefinedWord, error) {
	return refinedWords(s.db, sessionID, revision)
}

func refinedWords(q querier, sessionID string, revision int) ([]RefinedWord, error) {
	rows, err := q.Query(`SELECT recording_path, recording_sha256, recording_ordinal, ordinal, text, speaker, start_time, end_time
		FROM refined_words WHERE session_id = ? AND transcript_revision = ?
		ORDER BY recording_ordinal, ordinal`, sessionID, revision)
	if err != nil {
		return nil, fmt.Errorf("list refined words for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()
	var words []RefinedWord
	for rows.Next() {
		var word RefinedWord
		if err := rows.Scan(&word.RecordingPath, &word.RecordingSHA256, &word.RecordingOrdinal, &word.Ordinal, &word.Text, &word.Speaker, &word.Start, &word.End); err != nil {
			return nil, fmt.Errorf("scan refined word for session %s: %w", sessionID, err)
		}
		words = append(words, word)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate refined words for session %s: %w", sessionID, err)
	}
	return words, nil
}

// RefinedTranscript rebuilds transcript paragraphs from the retained batch
// words, retaining every recording's local order.
func RefinedTranscript(words []RefinedWord) string {
	var paragraphs []string
	recordingOrdinal := -1
	var text []string
	flush := func() {
		if len(text) > 0 {
			paragraphs = append(paragraphs, strings.Join(text, " "))
		}
		text = nil
	}
	for _, word := range words {
		if recordingOrdinal != -1 && word.RecordingOrdinal != recordingOrdinal {
			flush()
		}
		recordingOrdinal = word.RecordingOrdinal
		if word.Text = strings.TrimSpace(word.Text); word.Text != "" {
			text = append(text, word.Text)
		}
	}
	flush()
	return strings.Join(paragraphs, "\n\n")
}

// BuildRefinedSegments splits persisted word occurrences without rediscovering
// any text in the recording. A speaker change, sentence end or pause starts a
// new segment.
func BuildRefinedSegments(words []RefinedWord) []RefinedSegment {
	const pause = 800 * time.Millisecond
	const pauseTolerance = time.Millisecond
	segments := make([]RefinedSegment, 0)
	current := RefinedSegment{FirstWordOrdinal: -1}
	for _, word := range words {
		start := current.FirstWordOrdinal < 0
		if !start {
			gap := time.Duration((word.Start - current.End) * float64(time.Second))
			start = word.RecordingOrdinal != current.RecordingOrdinal ||
				word.RecordingPath != current.RecordingPath ||
				word.RecordingSHA256 != current.RecordingSHA256 ||
				word.Speaker != current.Speaker ||
				gap >= pause-pauseTolerance
		}
		if start {
			if current.FirstWordOrdinal >= 0 {
				segments = append(segments, current)
			}
			current = RefinedSegment{
				RecordingPath: word.RecordingPath, RecordingSHA256: word.RecordingSHA256, RecordingOrdinal: word.RecordingOrdinal,
				FirstWordOrdinal: word.Ordinal, LastWordOrdinal: word.Ordinal, Speaker: word.Speaker, Text: word.Text, Start: word.Start, End: word.End,
			}
			if refinedSentenceEnd(word.Text) {
				segments = append(segments, current)
				current.FirstWordOrdinal = -1
			}
			continue
		}
		current.Text = strings.TrimSpace(current.Text + " " + word.Text)
		current.LastWordOrdinal = word.Ordinal
		current.End = word.End
		if refinedSentenceEnd(word.Text) {
			segments = append(segments, current)
			current.FirstWordOrdinal = -1
		}
	}
	if current.FirstWordOrdinal >= 0 {
		segments = append(segments, current)
	}
	return segments
}

func refinedSentenceEnd(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasSuffix(text, ".") || strings.HasSuffix(text, "!") || strings.HasSuffix(text, "?")
}
