package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// askedQuestion is a turn of session sessionID as the Supervisor logs it.
func askedQuestion(sessionID string, at time.Time, outcome voice.QuestionOutcome) *voice.Question {
	firstAudio := 1250 * time.Millisecond
	return &voice.Question{
		SessionID: sessionID, AskedAt: at, HeardFrom: at.Add(-3 * time.Second), HeardTo: at.Add(-200 * time.Millisecond),
		Heard: "what is happening with ABC-12?", VendorWording: "What's the status of ABC-12?", Engine: "gptlive",
		IssueKey: "ABC-12", Outcome: outcome, Answer: "ABC-12: PR 80 is in review.", FirstAudio: &firstAudio, Took: 4200 * time.Millisecond,
	}
}

// TestTheQuestionLogKeepsEachTurn fails if a turn the Supervisor logs does
// not reach the unit's database as it was logged, for every outcome the
// voice gives: the voice's outcomes and the store's are one list.
func TestTheQuestionLogKeepsEachTurn(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", at); err != nil {
		t.Fatal(err)
	}
	stored := make([]string, 0, len(storage.VoiceQuestionOutcomes))
	for _, o := range storage.VoiceQuestionOutcomes {
		stored = append(stored, string(o))
	}
	spoken := make([]string, 0, len(voice.QuestionOutcomes))
	for _, o := range voice.QuestionOutcomes {
		spoken = append(spoken, string(o))
	}
	if !slices.Equal(stored, spoken) {
		t.Fatalf("the store keeps outcomes %q, the voice gives %q", stored, spoken)
	}
	var logged bytes.Buffer
	record := recordQuestion(store, slog.New(slog.NewTextHandler(&logged, nil)))
	for i, o := range voice.QuestionOutcomes {
		record(askedQuestion("s1", at.Add(time.Duration(i)*time.Minute), o))
	}
	got, err := storage.ReadVoiceQuestions(store.DB(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(voice.QuestionOutcomes) {
		t.Fatalf("kept %d questions, want %d; log:\n%s", len(got), len(voice.QuestionOutcomes), logged.String())
	}
	q := got[0]
	if q.SessionID != "s1" || !q.AskedAt.Equal(at) || !q.HeardFrom.Equal(at.Add(-3*time.Second)) || !q.HeardTo.Equal(at.Add(-200*time.Millisecond)) ||
		q.Heard != "what is happening with ABC-12?" || q.VendorWording != "What's the status of ABC-12?" || q.Engine != "gptlive" ||
		q.IssueKey != "ABC-12" || q.Outcome != storage.VoiceQuestionAnswered || q.Answer != "ABC-12: PR 80 is in review." ||
		q.FirstAudio == nil || *q.FirstAudio != 1250*time.Millisecond || q.Took != 4200*time.Millisecond {
		t.Errorf("kept %+v", q)
	}
}

// TestAQuestionTheStoreRefusesIsLoggedWithoutItsWords fails if a write the
// database refuses, or a turn with no recording session, panics, or leaves
// the room's words in the journal: the journal names the session and the
// outcome only.
func TestAQuestionTheStoreRefusesIsLoggedWithoutItsWords(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	var logged bytes.Buffer
	record := recordQuestion(store, slog.New(slog.NewTextHandler(&logged, nil)))
	record(askedQuestion("no-such-session", at, voice.OutcomeAnswered))
	record(askedQuestion("", at, voice.OutcomeAnswered))
	journal := logged.String()
	if strings.Count(journal, "level=WARN") != 2 || !strings.Contains(journal, "no-such-session") {
		t.Errorf("journal:\n%s\nwant a warning for each question left out, naming the session", journal)
	}
	for _, words := range []string{"ABC-12", "happening", "status", "review"} {
		if strings.Contains(journal, words) {
			t.Errorf("the journal holds the question's words (%q):\n%s", words, journal)
		}
	}
	if got, err := storage.ReadVoiceQuestions(store.DB(), time.Time{}); err != nil || len(got) != 0 {
		t.Errorf("kept %+v, %v; want nothing", got, err)
	}
}

// questionsUnit is a unit's config and database holding three questions of
// session s1, asked 2026-11-24 09:00 UTC, 2026-11-25 09:00 UTC and 09:01
// UTC, the last unanswered.
func questionsUnit(t *testing.T) string {
	t.Helper()
	configPath := firstAudioUnit(t, "")
	store, err := storage.NewSQLiteStore(filepath.Join(filepath.Dir(configPath), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", day.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	record := recordQuestion(store, slog.Default())
	record(askedQuestion("s1", day.Add(-24*time.Hour), voice.OutcomeAskedAgent))
	record(askedQuestion("s1", day, voice.OutcomeAnswered))
	stopped := askedQuestion("s1", day.Add(time.Minute), voice.OutcomeStopped)
	stopped.VendorWording, stopped.Answer, stopped.FirstAudio = "", "", nil
	record(stopped)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// TestVoiceQuestionsPrintsTheLog fails if ghost-wispr voice questions does
// not print every question of the unit's log, as JSON with --json and for a
// person without, in UTC, keeps only those asked from --since on, or changes
// any file of the unit's data directory.
func TestVoiceQuestionsPrintsTheLog(t *testing.T) {
	configPath := questionsUnit(t)
	before := dirState(t, filepath.Dir(configPath))

	var stdout, stderr bytes.Buffer
	if code := runVoiceCommand([]string{"voice", "questions", "--json"}, configPath, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("--json printed %q: %v", stdout.String(), err)
	}
	if len(rows) != 3 {
		t.Fatalf("--json printed %d questions, want 3: %s", len(rows), stdout.String())
	}
	want := map[string]any{
		"session_id": "s1", "asked_at": "2026-11-25T09:00:00Z", "heard_from": "2026-11-25T08:59:57Z", "heard_to": "2026-11-25T08:59:59.8Z",
		"heard": "what is happening with ABC-12?", "vendor_wording": "What's the status of ABC-12?", "engine": "gptlive",
		"issue_key": "ABC-12", "outcome": "answered", "answer": "ABC-12: PR 80 is in review.", "first_audio_ms": float64(1250), "took_ms": float64(4200),
	}
	for key, value := range want {
		if rows[1][key] != value {
			t.Errorf("--json question 2 %s = %#v, want %#v", key, rows[1][key], value)
		}
	}
	if id, ok := rows[1]["id"].(float64); !ok || id == 0 {
		t.Errorf("--json question 2 id = %#v", rows[1]["id"])
	}
	if rows[2]["first_audio_ms"] != nil || rows[2]["outcome"] != "stopped" {
		t.Errorf("--json question 3 = %v, want stopped with no first audio (null)", rows[2])
	}

	stdout.Reset()
	if code := runVoiceCommand([]string{"voice", "questions", "--since", "2026-11-25"}, configPath, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	text := stdout.String()
	for _, line := range []string{
		"2026-11-25T09:00:00Z  s1  gptlive  answered  ABC-12  first audio 1.25 s  took 4.20 s\n",
		"  heard:  what is happening with ABC-12?\n",
		"  vendor: What's the status of ABC-12?\n",
		"  answer: ABC-12: PR 80 is in review.\n",
		"2026-11-25T09:01:00Z  s1  gptlive  stopped  ABC-12  no audio  took 4.20 s\n",
		"2 questions\n",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("--since 2026-11-25 printed:\n%s\nwant the line %q", text, line)
		}
	}
	if strings.Contains(text, "asked_agent") || strings.Contains(text, "2026-11-24") {
		t.Errorf("--since 2026-11-25 printed a question asked before it:\n%s", text)
	}

	stdout.Reset()
	if code := runVoiceCommand([]string{"voice", "questions", "--json", "--since", "2026-11-26T00:00:00Z"}, configPath, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("a --since after every question: exit %d, printed %q; want []", code, stdout.String())
	}
	after := dirState(t, filepath.Dir(configPath))
	for name, was := range before {
		if now := after[name]; now != was {
			t.Errorf("the unit's %s was %s; after voice questions it is %q", name, was, now)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("voice questions left %s in the unit's data directory", name)
		}
	}

	// The unit runs: a question still in its write-ahead log is read too.
	unit, err := storage.NewSQLiteStore(filepath.Join(filepath.Dir(configPath), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unit.Close() })
	recordQuestion(unit, slog.Default())(askedQuestion("s1", time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC), voice.OutcomePicked))
	stdout.Reset()
	if code := runVoiceCommand([]string{"voice", "questions", "--json", "--since", "2026-11-25T09:30:00Z"}, configPath, &stdout, &stderr); code != 0 {
		t.Fatalf("beside the running unit: exit %d, stderr %q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0]["outcome"] != "picked" {
		t.Fatalf("beside the running unit printed %s (%v), want the question it just kept", stdout.String(), err)
	}
}

// TestVoiceQuestionsSaysWhatItCannotRead fails if a unit with no database, or
// one whose database has no question log, reads as one with no questions, or
// if wrong arguments do not exit 2 with the usage.
func TestVoiceQuestionsSaysWhatItCannotRead(t *testing.T) {
	configPath := firstAudioUnit(t, "")
	var stdout, stderr bytes.Buffer
	if code := runVoiceCommand([]string{"voice", "questions"}, configPath, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "no database") || stdout.Len() != 0 {
		t.Errorf("no database: exit %d, stdout %q, stderr %q; want exit 1 naming it", code, stdout.String(), stderr.String())
	}
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(configPath), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	stdout.Reset()
	stderr.Reset()
	if code := runVoiceCommand([]string{"voice", "questions", "--json"}, configPath, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "no question log") || stdout.Len() != 0 {
		t.Errorf("no question log: exit %d, stdout %q, stderr %q; want exit 1 naming it", code, stdout.String(), stderr.String())
	}
	for name, args := range map[string][]string{
		"no subcommand":      {"voice"},
		"another subcommand": {"voice", "answers"},
		"a bad date":         {"voice", "questions", "--since", "yesterday"},
		"an extra argument":  {"voice", "questions", "extra"},
	} {
		stderr.Reset()
		if code := runVoiceCommand(args, configPath, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: ghost-wispr voice questions") {
			t.Errorf("%s: exit %d, stderr %q; want 2 with the usage", name, code, stderr.String())
		}
	}
}
