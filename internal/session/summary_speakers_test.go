package session

import (
	"context"
	"encoding/json"
	"os"
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

// indexedTranscripts is a search indexer that hands on each transcript it
// is asked to index.
type indexedTranscripts chan string

func (c indexedTranscripts) IndexSessionAtGeneration(_ context.Context, _, transcript string, _ int) error {
	c <- transcript
	return nil
}

// TestASessionsSummaryReadsItsSpeakers fails if the summary a session's end
// makes, from its live segments or from its refinement, asks the model with
// a transcript that lacks each segment's speaker label, so the speaker ids it
// names are guesses from turn order; or if the transcript the session stores
// or search indexes gains the labels.
func TestASessionsSummaryReadsItsSpeakers(t *testing.T) {
	const id = "20261120100000"
	started := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		refine bool
		want   []string
	}{
		{"live segments", false, []string{
			"Speaker 0: Morning, Ben here. We move the launch from Thursday to Friday.",
			"Speaker 1: Hi Ben, it's Dana. QA found two bugs in the docking routine.",
		}},
		// The batch transcriber's words name no speaker, Deepgram's speaker 0.
		{"refinement", true, []string{"Speaker 0: Morning, Ben here."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			recording := filepath.Join(t.TempDir(), id+".mp3")
			if err := os.WriteFile(recording, []byte("audio"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateSession(id, started); err != nil {
				t.Fatal(err)
			}
			for i, seg := range []transcribe.Segment{
				{Speaker: 0, Text: "Morning, Ben here. We move the launch from Thursday to Friday."},
				{Speaker: 1, Text: "Hi Ben, it's Dana. QA found two bugs in the docking routine."},
				{Speaker: 0, Text: "Then QA gets one more day and the floor team hears today."},
			} {
				seg.StartTime, seg.EndTime, seg.Timestamp = float64(4*i), float64(4*i+3), started.Add(time.Duration(4*i)*time.Second)
				if err := store.AppendSegment(id, seg); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.EndSession(id, started.Add(time.Minute), recording, ""); err != nil {
				t.Fatal(err)
			}

			model := &promptRecorder{}
			summarizer := summary.New(config.Summarization{Model: "gemini/gemini-4-carbon", Presets: map[string]config.Preset{"default": {
				Description: "general", UserTemplate: "{{transcript}}",
			}}}, func(_, _ string) (llm.Client, error) { return model, nil })
			indexed := make(indexedTranscripts, 1)
			manager := NewManager(store, nil, summarizer, &hubMock{}, NewDetector(time.Hour), 0)
			manager.SetIndexer(indexed)
			if tc.refine {
				manager.SetBatchTranscriber(batchTranscriberMock{transcript: "Morning, Ben here. We move the launch from Thursday to Friday, and QA gets one more day to fix the two docking bugs, refined."})
				err = manager.RefineSession(context.Background(), id, recording, started)
			} else {
				err = manager.SummarizeSession(context.Background(), id, started)
			}
			if err != nil {
				t.Fatal(err)
			}

			prompt := model.last()
			var missing []string
			for _, want := range tc.want {
				if !strings.Contains(prompt, want) {
					missing = append(missing, want)
				}
			}
			if len(missing) > 0 {
				t.Errorf("the summary's prompt lacks %q:\n%s", missing, prompt)
			}
			var searched string
			select {
			case searched = <-indexed:
			case <-time.After(5 * time.Second):
				t.Fatal("the session was not indexed for search")
			}
			sess, err := store.GetSession(id)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(searched, "Speaker") || strings.Contains(sess.CanonicalTranscript, "Speaker") || !strings.Contains(sess.CanonicalTranscript, "Morning, Ben here.") {
				t.Errorf("search indexed %q and the session stores %q; want the canonical transcript, with no speaker labels", searched, sess.CanonicalTranscript)
			}
		})
	}
}
