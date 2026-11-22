package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// backlogMessageWord is a word only the refusing provider's message holds:
// an announcement that quotes the message would show it.
const backlogMessageWord = "backlogplantedmessage"

// alphaOnlyModel summarizes a transcript that holds "alphaword", and refuses
// any other with HTTP 400, a refusal no retry mends.
type alphaOnlyModel struct{}

func (alphaOnlyModel) Complete(context.Context, []llm.Message) (string, error) {
	return "", providererr.New("fake", 400, "invalid_request_error", backlogMessageWord)
}

func (alphaOnlyModel) CompleteJSON(_ context.Context, messages []llm.Message, _ map[string]any) (json.RawMessage, error) {
	if strings.Contains(messages[len(messages)-1].Content, "alphaword") {
		return json.RawMessage(`{"title":"Alpha bridge","summary":"We talked about the alpha bridge.","speakers":[]}`), nil
	}
	return nil, providererr.New("fake", 400, "invalid_request_error", backlogMessageWord)
}

// TestABacklogSummaryReachesThePage fails if a summary the backlog makes
// online (the start's backlog, or POST /api/sessions/batch-backfill gone
// online, as newOnlineSummaries runs them) is not announced to the page as
// the other summary paths announce theirs: running when it starts, then the
// new summary when it is stored, or, when it fails, the title and summary
// the session keeps with the reason it stored in summary_error. Without
// them the page shows a new summary, or why one failed, only after a reload.
func TestABacklogSummaryReachesThePage(t *testing.T) {
	cfg := config.Config{
		Providers:    map[string]config.Provider{"fake": {API: config.APIOpenAI, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}}},
		ProviderKeys: map[string]string{"EXAMPLE_KEY": "k-1"},
		Summarization: config.Summarization{
			Model: "fake/model", MinSummaryWords: 1,
			Presets: map[string]config.Preset{"default": {Description: "Summary", UserTemplate: "{{transcript}}"}},
		},
	}
	summaries := newSummarizers(func(_, _ string) (llm.Client, error) { return alphaOnlyModel{}, nil }, func(error) {})
	if summaries.apply(&cfg) == nil {
		t.Fatal("the test config cannot summarize")
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "backlog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for i, word := range []string{"alphaword", "bravoword"} {
		id := "s-" + word
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendSegment(id, transcribe.Segment{Text: "We talked about the " + word + " bridge.", EndTime: 2, Timestamp: at}); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	// The second session's last summary failed; it keeps its last good one.
	if _, err := store.DB().Exec(`UPDATE sessions SET title = 'Planted', summary = 'Last good summary.', summary_status = ? WHERE id = 's-bravoword'`, storage.SummaryFailed); err != nil {
		t.Fatal(err)
	}

	hub := &recordingBroadcaster{}
	newOnlineSummaries(store, summaries.summarizer, hub, func(string) {})(t.Context(), []string{"s-alphaword", "s-bravoword"})

	failed, err := store.GetSession("s-bravoword")
	if err != nil {
		t.Fatal(err)
	}
	if failed.SummaryStatus != storage.SummaryFailed || !strings.Contains(failed.SummaryError, "HTTP 400 (invalid_request_error)") {
		t.Fatalf("the refused session: summary_status %q, summary_error %q; want failed with the provider's status and code", failed.SummaryStatus, failed.SummaryError)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	announced := map[string][]summaryEvent{}
	for _, event := range hub.events {
		announced[event.sessionID] = append(announced[event.sessionID], event)
	}
	for id, want := range map[string][]summaryEvent{
		"s-alphaword": {
			{"s-alphaword", "", "", storage.SummaryRunning, "", ""},
			{"s-alphaword", "Alpha bridge", "We talked about the alpha bridge.", storage.SummaryCompleted, "default", ""},
		},
		"s-bravoword": {
			{"s-bravoword", "", "", storage.SummaryRunning, "", ""},
			{"s-bravoword", "Planted", "Last good summary.", storage.SummaryFailed, "", failed.SummaryError},
		},
	} {
		got := announced[id]
		if len(got) != len(want) {
			t.Errorf("session %s was announced %+v, want %+v", id, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("session %s announcement %d = %+v, want %+v", id, i, got[i], want[i])
			}
		}
	}
	for _, event := range hub.events {
		if strings.Contains(event.reason, backlogMessageWord) {
			t.Errorf("an announcement quotes the provider's message: %+v", event)
		}
	}
}
