package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// promptRecorder is a summary model that keeps the user message of each
// request and answers with a summary that names no one.
type promptRecorder struct {
	mu      sync.Mutex
	prompts []string
}

func (p *promptRecorder) record(messages []llm.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prompts = append(p.prompts, messages[len(messages)-1].Content)
}

func (p *promptRecorder) Complete(_ context.Context, messages []llm.Message) (string, error) {
	p.record(messages)
	return "The team moved the launch.", nil
}

func (p *promptRecorder) CompleteJSON(_ context.Context, messages []llm.Message, _ map[string]any) (json.RawMessage, error) {
	p.record(messages)
	return json.RawMessage(`{"title":"Launch moved","summary":"The team moved the launch.","speakers":[]}`), nil
}

func (p *promptRecorder) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.prompts) == 0 {
		return ""
	}
	return p.prompts[len(p.prompts)-1]
}

// speakerSummaryID is the session speakerSessionStore holds.
const speakerSummaryID = "20261120110000"

// speakerTranscriptLines are the lines a summary of speakerSummaryID must
// read: each segment under its speaker's label.
var speakerTranscriptLines = []string{
	"Speaker 0: Morning, Ben here. We move the launch from Thursday to Friday.",
	"Speaker 1: Hi Ben, it's Dana. QA found two bugs in the docking routine.",
	"Speaker 0: Then QA gets one more day and the floor team hears today.",
}

// speakerSessionStore is a store with one ended session, speakerSummaryID,
// whose live segments are two people's.
func speakerSessionStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	if err := store.CreateSession(speakerSummaryID, started); err != nil {
		t.Fatal(err)
	}
	for i, line := range speakerTranscriptLines {
		speaker, text, _ := strings.Cut(strings.TrimPrefix(line, "Speaker "), ": ")
		seg := transcribe.Segment{Speaker: int(speaker[0] - '0'), Text: text, StartTime: float64(4 * i), EndTime: float64(4*i + 3), Timestamp: started.Add(time.Duration(4*i) * time.Second)}
		if err := store.AppendSegment(speakerSummaryID, seg); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(speakerSummaryID, started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestEverySummaryPathReadsTheSpeakers fails if a path that asks a model for
// a session's summary (making it again, with a preset or without; the
// start's backlog and POST /api/sessions/batch-backfill online; a merged
// session's; a summary batch job's requests; the settings page's test of a
// preset) gives the model a transcript without each segment's speaker label,
// so the speaker ids it names are guesses from turn order; or if the session
// it stores the summary for gains the labels in its canonical transcript.
func TestEverySummaryPathReadsTheSpeakers(t *testing.T) {
	presets := map[string]config.Preset{"default": {Description: "general", UserTemplate: "{{transcript}}"}}
	cfg := config.Summarization{Model: "gemini/gemini-4-carbon", Presets: presets}
	for _, tc := range []struct {
		name string
		ask  func(t *testing.T, store *storage.SQLiteStore, summarizer *summary.Summarizer, model *promptRecorder) string
	}{
		{"resummarize", func(t *testing.T, store *storage.SQLiteStore, summarizer *summary.Summarizer, model *promptRecorder) string {
			if err := resummarize(context.Background(), store, summarizer, &recordingBroadcaster{}, func(string) {}, speakerSummaryID, ""); err != nil {
				t.Fatal(err)
			}
			return model.last()
		}},
		{"resummarize with a preset", func(t *testing.T, store *storage.SQLiteStore, summarizer *summary.Summarizer, model *promptRecorder) string {
			if err := resummarize(context.Background(), store, summarizer, &recordingBroadcaster{}, func(string) {}, speakerSummaryID, "default"); err != nil {
				t.Fatal(err)
			}
			return model.last()
		}},
		{"backlog online", func(_ *testing.T, store *storage.SQLiteStore, summarizer *summary.Summarizer, model *promptRecorder) string {
			summarizeBackfillSession(context.Background(), store, summarizer, &recordingBroadcaster{}, func(string) {}, speakerSummaryID)
			return model.last()
		}},
		{"merged session", func(_ *testing.T, store *storage.SQLiteStore, summarizer *summary.Summarizer, model *promptRecorder) string {
			summarizeMergedSession(context.Background(), store, summarizer, &recordingBroadcaster{}, func(string) {}, speakerSummaryID)
			return model.last()
		}},
		{"summary batch job", func(t *testing.T, store *storage.SQLiteStore, summarizer *summary.Summarizer, _ *promptRecorder) string {
			sessions, _, _, err := readBatchSummarySessions(store, []string{speakerSummaryID})
			if err != nil {
				t.Fatal(err)
			}
			requests, err := summarizer.BuildBatchRequests(context.Background(), sessions)
			if err != nil || len(requests) != 1 {
				t.Fatalf("BuildBatchRequests = %d requests, %v; want 1", len(requests), err)
			}
			return requests[0].Contents[len(requests[0].Contents)-1].Parts[0].Text
		}},
		{"preset test", func(t *testing.T, store *storage.SQLiteStore, _ *summary.Summarizer, model *promptRecorder) string {
			held := &heldConfig{cfg: config.Config{Summarization: cfg}}
			test := presetTester(held, store, func(_, _ string) (llm.Client, error) { return model, nil })
			if _, err := test(context.Background(), "default", speakerSummaryID); err != nil {
				t.Fatal(err)
			}
			return model.last()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := speakerSessionStore(t)
			model := &promptRecorder{}
			summarizer := summary.New(cfg, func(_, _ string) (llm.Client, error) { return model, nil })

			prompt := tc.ask(t, store, summarizer, model)
			var missing []string
			for _, want := range speakerTranscriptLines {
				if !strings.Contains(prompt, want) {
					missing = append(missing, want)
				}
			}
			if len(missing) > 0 {
				t.Errorf("the model was asked without the lines %q:\n%s", missing, prompt)
			}
			sess, err := store.GetSession(speakerSummaryID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(sess.CanonicalTranscript, "Speaker") {
				t.Errorf("the stored canonical transcript gained speaker labels: %q", sess.CanonicalTranscript)
			}
		})
	}
}
