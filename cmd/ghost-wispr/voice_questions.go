package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// questionStore is where the question log keeps each question;
// *storage.SQLiteStore is one.
type questionStore interface {
	RecordVoiceQuestion(q *storage.VoiceQuestion) error
}

// recordQuestion is the meeting voice's question log on store
// (voice.SupervisorConfig.Question): each turn the Concierge handled, kept in
// voice_questions once its outcome is known. The Supervisor calls it on a
// goroutine of its own, after the answer, so nothing here reaches the room. A
// question with no recording session (its exchange ended before its session
// settled) or one the store refuses is left out with a warning that names
// the session and the outcome, never the room's words; one whose words a
// segment deletion erased before the write came is left out as it should be,
// with a note.
func recordQuestion(store questionStore, logger *slog.Logger) func(*voice.Question) {
	return func(q *voice.Question) {
		if q.SessionID == "" {
			logger.Warn("voice: a question with no recording session is left out of the question log", "outcome", q.Outcome)
			return
		}
		if err := store.RecordVoiceQuestion(&storage.VoiceQuestion{
			SessionID: q.SessionID, AskedAt: q.AskedAt, HeardFrom: q.HeardFrom, HeardTo: q.HeardTo,
			Heard: q.Heard, VendorWording: q.VendorWording, Engine: q.Engine, IssueKey: q.IssueKey,
			Outcome: storage.VoiceQuestionOutcome(q.Outcome), Answer: q.Answer, FirstAudio: q.FirstAudio, Took: q.Took,
		}); errors.Is(err, storage.ErrVoiceQuestionErased) {
			logger.Info("voice: a question heard in a segment deleted since is left out of the question log", "session", q.SessionID, "outcome", q.Outcome)
		} else if err != nil {
			logger.Warn("voice: the question log could not keep a question", "session", q.SessionID, "outcome", q.Outcome, "error", err)
		}
	}
}

const voiceQuestionsUsage = "usage: ghost-wispr voice questions [--since DATE] [--json]\n" +
	"  Prints the meeting voice's question log, oldest first: each turn the Concierge handled, as heard after the\n" +
	"  wake phrase, a driving voice's own wording of it, the voice, the issue taken, how it ended, the answer, and\n" +
	"  the time to its first audio and to its end, all times in UTC. --since keeps the questions asked from DATE on\n" +
	"  (2026-11-25, from midnight UTC, or an RFC 3339 time). --json prints one JSON array. It reads the unit's\n" +
	"  database and writes nothing."

// runVoiceCommand is ghost-wispr voice questions: the question log, the test
// bed of the Concierge's answers, read from the unit's database without
// writing to it, beside the running unit. A database it cannot read, or one
// with no question log, exits 1 with why; wrong arguments exit 2.
func runVoiceCommand(args []string, configPath string, stdout, stderr io.Writer) int {
	usage := func(problem string) int {
		_, _ = fmt.Fprintf(stderr, "voice: %s\n%s\n", problem, voiceQuestionsUsage)
		return 2
	}
	if len(args) < 2 || args[1] != "questions" {
		return usage("the subcommand is questions")
	}
	flags := flag.NewFlagSet("voice questions", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sinceText := flags.String("since", "", "")
	asJSON := flags.Bool("json", false, "")
	if err := flags.Parse(args[2:]); err != nil {
		return usage(err.Error())
	}
	if flags.NArg() > 0 {
		return usage(fmt.Sprintf("unexpected arguments %q", flags.Args()))
	}
	since, err := parseSince(*sinceText)
	if err != nil {
		return usage(err.Error())
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "voice questions: %v\n", err)
		return 1
	}
	cfgStore, _, err := loadRuntimeConfig(configPath)
	if err != nil {
		return fail(fmt.Errorf("config: %w", err))
	}
	dbPath, err := databasePath(cfgStore.Get().DBPath)
	if err != nil {
		return fail(err)
	}
	questions, err := readQuestionLog(dbPath, since)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		err = printQuestionsJSON(stdout, questions)
	} else {
		err = printQuestions(stdout, questions)
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

// parseSince reads --since: a date, from its midnight UTC, or an RFC 3339
// time; "" is every question.
func parseSince(text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	if day, err := time.Parse(time.DateOnly, text); err == nil {
		return day, nil
	}
	if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return at, nil
	}
	return time.Time{}, fmt.Errorf("--since %q is neither a date (2026-11-25) nor an RFC 3339 time", text)
}

// readQuestionLog reads the questions asked from since on from the unit's
// database at dbPath, through a read-only connection: the running unit owns
// the database, and opening it as the unit does backs it up and finishes the
// unit's erases. While the unit runs, its write-ahead log is there and the
// connection reads through it. With no log, the unit is stopped and its
// database whole; a read-only connection would leave an empty log behind,
// so the database is read as immutable, which opens no file beside it. A
// unit that starts within that read, a few milliseconds, could checkpoint
// under it; the command is for a person or an agent beside a running unit.
func readQuestionLog(dbPath string, since time.Time) ([]storage.VoiceQuestion, error) {
	if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no database at %s: the unit has kept nothing yet", dbPath)
	} else if err != nil {
		return nil, err
	}
	query := "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	if _, err := os.Stat(dbPath + "-wal"); errors.Is(err, fs.ErrNotExist) {
		query = "mode=ro&immutable=1&_pragma=query_only(1)"
	} else if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: dbPath, RawQuery: query}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	return storage.ReadVoiceQuestions(db, since)
}

// questionLine is one question as --json prints it; first_audio_ms is null
// when no audio played.
type questionLine struct {
	ID            int64  `json:"id"`
	SessionID     string `json:"session_id"`
	AskedAt       string `json:"asked_at"`
	HeardFrom     string `json:"heard_from"`
	HeardTo       string `json:"heard_to"`
	Heard         string `json:"heard"`
	VendorWording string `json:"vendor_wording"`
	Engine        string `json:"engine"`
	IssueKey      string `json:"issue_key"`
	Outcome       string `json:"outcome"`
	Answer        string `json:"answer"`
	FirstAudioMS  *int64 `json:"first_audio_ms"`
	TookMS        int64  `json:"took_ms"`
}

func printQuestionsJSON(w io.Writer, questions []storage.VoiceQuestion) error {
	lines := make([]questionLine, 0, len(questions))
	utc := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	for i := range questions {
		q := &questions[i]
		line := questionLine{
			ID: q.ID, SessionID: q.SessionID, AskedAt: utc(q.AskedAt), HeardFrom: utc(q.HeardFrom), HeardTo: utc(q.HeardTo),
			Heard: q.Heard, VendorWording: q.VendorWording, Engine: q.Engine, IssueKey: q.IssueKey,
			Outcome: string(q.Outcome), Answer: q.Answer, TookMS: q.Took.Milliseconds(),
		}
		if q.FirstAudio != nil {
			ms := q.FirstAudio.Milliseconds()
			line.FirstAudioMS = &ms
		}
		lines = append(lines, line)
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(lines)
}

// printQuestions prints each question for a person: its time, session, voice,
// outcome, issue and timings on one line, then what was heard, the vendor's
// wording and the answer, and last how many questions there were.
func printQuestions(w io.Writer, questions []storage.VoiceQuestion) error {
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for i := range questions {
		q := &questions[i]
		audio := "no audio"
		if q.FirstAudio != nil {
			audio = fmt.Sprintf("first audio %.2f s", q.FirstAudio.Seconds())
		}
		if _, err := fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s  took %.2f s\n  heard:  %s\n",
			q.AskedAt.UTC().Format(time.RFC3339), q.SessionID, dash(q.Engine), q.Outcome, dash(q.IssueKey), audio, q.Took.Seconds(), q.Heard); err != nil {
			return err
		}
		for _, field := range []struct{ name, text string }{{"vendor", q.VendorWording}, {"answer", q.Answer}} {
			if field.text == "" {
				continue
			}
			if _, err := fmt.Fprintf(w, "  %s: %s\n", field.name, field.text); err != nil {
				return err
			}
		}
	}
	count := fmt.Sprintf("%d questions", len(questions))
	if len(questions) == 1 {
		count = "1 question"
	}
	_, err := fmt.Fprintln(w, count)
	return err
}
