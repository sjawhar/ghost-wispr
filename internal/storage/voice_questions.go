package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
)

// VoiceQuestionOutcome is how one turn the Concierge handled ended, as the
// question log keeps it.
type VoiceQuestionOutcome string

const (
	// VoiceQuestionAnswered: Wispr answered from Dispatch's record.
	VoiceQuestionAnswered VoiceQuestionOutcome = "answered"
	// VoiceQuestionAskedBack: Wispr asked the person which issue they meant
	// ("Did you mean …?"), or its key.
	VoiceQuestionAskedBack VoiceQuestionOutcome = "asked_back"
	// VoiceQuestionPicked: the turn picked one of the issues Wispr offered,
	// and Wispr answered the question first asked, about it.
	VoiceQuestionPicked VoiceQuestionOutcome = "picked"
	// VoiceQuestionSideTalk: a reply to Wispr's own question that asked
	// nothing; no voice answered it.
	VoiceQuestionSideTalk VoiceQuestionOutcome = "side_talk"
	// VoiceQuestionOutage: Dispatch or Envoy did not answer.
	VoiceQuestionOutage VoiceQuestionOutcome = "outage"
	// VoiceQuestionAskedAgent: the question went to the agent that holds
	// the issue.
	VoiceQuestionAskedAgent VoiceQuestionOutcome = "asked_agent"
	// VoiceQuestionStopped: a stop or a barge-in cut the answer.
	VoiceQuestionStopped VoiceQuestionOutcome = "stopped"
	// VoiceQuestionNeverMind: "Wispr, never mind" cut the answer, or the
	// person declined the issues Wispr offered ("no, neither").
	VoiceQuestionNeverMind VoiceQuestionOutcome = "never_mind"
	// VoiceQuestionFailed: the Concierge's model or the voice failed.
	VoiceQuestionFailed VoiceQuestionOutcome = "failed"
)

// VoiceQuestionOutcomes are every outcome the question log keeps.
var VoiceQuestionOutcomes = []VoiceQuestionOutcome{
	VoiceQuestionAnswered, VoiceQuestionAskedBack, VoiceQuestionPicked, VoiceQuestionSideTalk, VoiceQuestionOutage,
	VoiceQuestionAskedAgent, VoiceQuestionStopped, VoiceQuestionNeverMind, VoiceQuestionFailed,
}

// VoiceQuestion is one row of the voice_questions log: a turn the Concierge
// handled, the test bed of its answers. It holds the room's words, and goes
// with its session's delete, or with a segment deletion whose segment its
// words were heard in (eraseQuestionsHeardIn).
type VoiceQuestion struct {
	ID        int64
	SessionID string
	// AskedAt is when the turn was handed to the Concierge; FirstAudio and
	// Took count from it.
	AskedAt time.Time
	// HeardFrom and HeardTo are when the turn's words, the wake phrase's
	// included, were said, by their capture times.
	HeardFrom, HeardTo time.Time
	// Heard is the turn as the Supervisor handed it to the Concierge, after
	// the wake phrase, as Deepgram heard it.
	Heard string
	// VendorWording is a driving voice's own wording of the turn, the
	// question its vendor asked the Concierge, when it differs from Heard.
	VendorWording string
	// Engine is the voice, as voice.engine names it ("tts", "gptlive" or
	// "dgagent").
	Engine   string
	IssueKey string // the issue the Concierge took, or ""
	Outcome  VoiceQuestionOutcome
	// Answer is what Wispr was given to say for the turn, its lines in order.
	Answer string
	// FirstAudio is how long after AskedAt the answer's first audio played;
	// nil when none played.
	FirstAudio *time.Duration
	Took       time.Duration // how long after AskedAt the outcome was known
}

const voiceQuestionColumns = `id, session_id, asked_at, heard_from, heard_to, heard, vendor_wording, engine, issue_key, outcome, answer, first_audio_ms, took_ms`

// ErrNoQuestionLog is ReadVoiceQuestions' error for a database with no
// question log: a build before it made the database.
var ErrNoQuestionLog = errors.New("the database has no question log (voice_questions): the build that made it keeps no questions")

// RecordVoiceQuestion records one question. Its session must exist, and its
// outcome be one of VoiceQuestionOutcomes. Times keep the nanosecond and
// durations the millisecond.
func (s *SQLiteStore) RecordVoiceQuestion(q *VoiceQuestion) error {
	if !slices.Contains(VoiceQuestionOutcomes, q.Outcome) {
		return fmt.Errorf("record a question of session %s: outcome %q is none the log keeps", q.SessionID, q.Outcome)
	}
	var firstAudio any
	if q.FirstAudio != nil {
		firstAudio = q.FirstAudio.Milliseconds()
	}
	if _, err := s.db.Exec(`INSERT INTO voice_questions(session_id, asked_at, heard_from, heard_to, heard, vendor_wording, engine, issue_key, outcome, answer, first_audio_ms, took_ms)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.SessionID, questionTime(q.AskedAt), questionTime(q.HeardFrom), questionTime(q.HeardTo),
		q.Heard, q.VendorWording, q.Engine, q.IssueKey, q.Outcome, q.Answer, firstAudio, q.Took.Milliseconds()); err != nil {
		return fmt.Errorf("record a question of session %s: %w", q.SessionID, err)
	}
	return nil
}

// questionTime is the text form of a question's times: voiceTaskTime's, in
// UTC, so text order is time order.
func questionTime(t time.Time) string {
	return t.UTC().Format(voiceTaskTime)
}

// ReadVoiceQuestions lists the questions asked at or after since, as asked,
// from db: the store's, or a read-only connection to the unit's database
// (ghost-wispr voice questions). A database with no question log is
// ErrNoQuestionLog, never an empty list.
func ReadVoiceQuestions(db *sql.DB, since time.Time) ([]VoiceQuestion, error) {
	switch err := db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'voice_questions'`).Scan(new(int)); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNoQuestionLog
	case err != nil:
		return nil, fmt.Errorf("read the question log: %w", err)
	}
	rows, err := db.Query(`SELECT `+voiceQuestionColumns+` FROM voice_questions WHERE asked_at >= ? ORDER BY asked_at, id`, questionTime(since))
	if err != nil {
		return nil, fmt.Errorf("read the question log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	questions := []VoiceQuestion{}
	for rows.Next() {
		var q VoiceQuestion
		var askedAt, heardFrom, heardTo, outcome string
		var firstAudio sql.NullInt64
		var took int64
		if err := rows.Scan(&q.ID, &q.SessionID, &askedAt, &heardFrom, &heardTo, &q.Heard, &q.VendorWording, &q.Engine, &q.IssueKey, &outcome, &q.Answer, &firstAudio, &took); err != nil {
			return nil, fmt.Errorf("read the question log: %w", err)
		}
		for _, field := range []struct {
			text string
			to   *time.Time
		}{{askedAt, &q.AskedAt}, {heardFrom, &q.HeardFrom}, {heardTo, &q.HeardTo}} {
			if *field.to, err = time.Parse(time.RFC3339Nano, field.text); err != nil {
				return nil, fmt.Errorf("read question %d of the log: %w", q.ID, err)
			}
		}
		q.Outcome = VoiceQuestionOutcome(outcome)
		if firstAudio.Valid {
			d := time.Duration(firstAudio.Int64) * time.Millisecond
			q.FirstAudio = &d
		}
		q.Took = time.Duration(took) * time.Millisecond
		questions = append(questions, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the question log: %w", err)
	}
	return questions, nil
}

// questionHeardSlack widens a deleted segment's span on each side when it is
// matched against the questions' heard spans. Both are on the capture clock
// (a recording begins at its session's started_at, session.Manager's
// recordingStart, and a question's words are timed as the live stream
// captured them), but each by its own reading of it; a question heard at a
// segment's edge goes with it.
const questionHeardSlack = time.Second

// untimedSegmentLag is how long after its last word an untimed segment, of a
// merged source with no recording, can have been stored: its timestamp is
// when its live result was stored, after Deepgram's endpoint or utterance end
// (400 ms and 1000 ms on the unit) and the result's own way back.
const untimedSegmentLag = 3 * time.Second

// eraseQuestionsHeardIn deletes, in tx, the question log's rows of sessions
// whose heard span overlaps from to to, widened by questionHeardSlack: the
// questions whose words a segment deletion took off the unit.
func eraseQuestionsHeardIn(tx *sql.Tx, sessions []string, from, to time.Time) error {
	placeholders, args := placeholderList(sessions)
	args = append(args, questionTime(to.Add(questionHeardSlack)), questionTime(from.Add(-questionHeardSlack)))
	if _, err := tx.Exec(`DELETE FROM voice_questions WHERE session_id IN (`+placeholders+`) AND heard_from <= ? AND heard_to >= ?`, args...); err != nil {
		return fmt.Errorf("erase the questions heard in the deleted segment: %w", err)
	}
	return nil
}

// eraseQuestionsHeardInRecording erases, in tx, the questions of sessions
// heard from start to end seconds into recording, a recording of session
// sessionID (eraseQuestionsHeardIn).
func eraseQuestionsHeardInRecording(tx *sql.Tx, sessionID, recording string, start, end float64, sessions []string) error {
	began, err := recordingStart(tx, sessionID, recording)
	if err != nil {
		return err
	}
	return eraseQuestionsHeardIn(tx, sessions, began.Add(seconds(start)), began.Add(seconds(end)))
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// recordingStart is when recording, a recording of session sessionID, began
// on the wall clock: the started_at of the session that recorded it, found
// down the merges through recording_sources (a session's recording begins at
// its started_at: session.Manager's recordingStart). A recording no row names
// is sessionID's own.
func recordingStart(tx *sql.Tx, sessionID, recording string) (time.Time, error) {
	owner, seen := sessionID, map[string]bool{}
	for !seen[owner] {
		seen[owner] = true
		next, err := recordingSource(tx, owner, recording)
		if err != nil {
			return time.Time{}, err
		}
		if next == "" {
			break
		}
		owner = next
	}
	session, err := getSession(tx, owner)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the session that recorded %s: %w", recording, err)
	}
	return session.StartedAt, nil
}

// recordingSource is the session merged session merged took recording from,
// by its path or its name (recording.Key); "" when none.
func recordingSource(tx *sql.Tx, merged, recording string) (string, error) {
	rows, err := tx.Query(`SELECT recording_path, source_session_id FROM recording_sources WHERE merged_session_id = ?`, merged)
	if err != nil {
		return "", fmt.Errorf("read the recordings merged into %s: %w", merged, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var path, source string
		if err := rows.Scan(&path, &source); err != nil {
			return "", fmt.Errorf("read the recordings merged into %s: %w", merged, err)
		}
		if path == recording || recordingname.Key(path) == recordingname.Key(recording) {
			return source, nil
		}
	}
	return "", rows.Err()
}
