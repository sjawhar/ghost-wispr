package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestSolRecheck195EveryOutcomeIsExported fails if any outcome the voice
// gives is not printed exactly once by ghost-wispr voice questions, as JSON
// and as text, for one question of each, or if the store takes an outcome
// the log does not know. (Sol's review of #195 at 14fd94eb.)
func TestSolRecheck195EveryOutcomeIsExported(t *testing.T) {
	configPath := firstAudioUnit(t, "")
	store, err := storage.NewSQLiteStore(filepath.Join(filepath.Dir(configPath), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", at); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordVoiceQuestion(&storage.VoiceQuestion{SessionID: "s1", AskedAt: at, Outcome: "unknown-outcome"}); err == nil {
		t.Fatal("control: the store took an outcome the log does not know")
	}
	record := recordQuestion(store, slog.Default())
	for i, outcome := range voice.QuestionOutcomes {
		record(askedQuestion("s1", at.Add(time.Duration(i)*time.Second), outcome))
	}
	var out, stderr bytes.Buffer
	if code := runVoiceCommand([]string{"voice", "questions", "--json"}, configPath, &out, &stderr); code != 0 {
		t.Fatalf("--json: exit %d, stderr %q", code, stderr.String())
	}
	var rows []questionLine
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, row := range rows {
		got[row.Outcome]++
	}
	if len(rows) != len(voice.QuestionOutcomes) || len(got) != len(voice.QuestionOutcomes) {
		t.Fatalf("--json printed %d rows of %d outcomes, want %d of each: %v", len(rows), len(got), len(voice.QuestionOutcomes), got)
	}
	out.Reset()
	if code := runVoiceCommand([]string{"voice", "questions"}, configPath, &out, &stderr); code != 0 {
		t.Fatalf("text: exit %d, stderr %q", code, stderr.String())
	}
	for _, outcome := range voice.QuestionOutcomes {
		if got[string(outcome)] != 1 || !strings.Contains(out.String(), "  "+string(outcome)+"  ") {
			t.Errorf("outcome %q is not printed once in each form; --json %v; text:\n%s", outcome, got, out.String())
		}
	}
}
