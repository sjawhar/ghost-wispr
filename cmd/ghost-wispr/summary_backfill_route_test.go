package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// heldModel is a summary model that holds each call until release is
// closed, and counts the calls for each session by the word its transcript
// holds.
type heldModel struct {
	release chan struct{}
	mu      sync.Mutex
	calls   map[string]int
	asked   chan string
}

func (h *heldModel) Complete(context.Context, []llm.Message) (string, error) {
	return "", fmt.Errorf("no preset choice in this test")
}

func (h *heldModel) CompleteJSON(ctx context.Context, messages []llm.Message, _ map[string]any) (json.RawMessage, error) {
	prompt := messages[len(messages)-1].Content
	word := ""
	for i := 1; i <= 6; i++ {
		if w := fmt.Sprintf("word%d", i); strings.Contains(prompt, w+" ") {
			word = w
		}
	}
	h.mu.Lock()
	h.calls[word]++
	h.mu.Unlock()
	select {
	case h.asked <- word:
	default:
	}
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return json.RawMessage(`{"title":"Talk of ` + word + `","summary":"They talked about ` + word + `.","speakers":[]}`), nil
}

func (h *heldModel) count(word string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[word]
}

// TestTheOnlineBackfillRouteSummarizesAClaimedSessionOnce fails if POST
// /api/sessions/backfill, with the start's online backlog running beside
// it, summarizes a session the backlog claimed after the route listed it,
// as GPT-6.1 Sol's review of e9c6fb8 found: the route queued every session
// and made each summary again unconditionally (Resummarize), so the sixth
// session, which waited for one of the route's five slots while the backlog
// claimed it, was summarized twice. The route must claim each summary as
// the backlogs do.
func TestTheOnlineBackfillRouteSummarizesAClaimedSessionOnce(t *testing.T) {
	cfg := config.Config{
		Providers:    map[string]config.Provider{"fake": {API: config.APIOpenAI, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}}},
		ProviderKeys: map[string]string{"EXAMPLE_KEY": "k-1"},
		Summarization: config.Summarization{
			Model: "fake/model", MinSummaryWords: 1,
			Presets: map[string]config.Preset{"default": {Description: "Summary", UserTemplate: "{{transcript}}"}},
		},
	}
	model := &heldModel{release: make(chan struct{}), calls: map[string]int{}, asked: make(chan string, 16)}
	t.Cleanup(func() {
		select {
		case <-model.release:
		default:
			close(model.release)
		}
	})
	summaries := newSummarizers(func(_, _ string) (llm.Client, error) { return model, nil }, func(error) {})
	if summaries.apply(&cfg) == nil {
		t.Fatal("the test config cannot summarize")
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "route.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("s-%d", i)
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendSegment(id, transcribe.Segment{Text: fmt.Sprintf("We talked about word%d today.", i), EndTime: 2, Timestamp: at}); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	hub := &recordingBroadcaster{}
	manager := stopTestManager(t, store)
	online := newOnlineSummaries(store, summaries.summarizer, hub, func(string) {})
	// The hooks as main wires them (wireStoreWork).
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, &server.ControlHooks{
		Resummarize:      resummarizeUntilStop(manager, store, summaries.summarizer, hub, func(string) {}),
		SummarizeBacklog: backlogUntilStop(manager, online),
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/backfill", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"found":6`) {
		t.Fatalf("POST /api/sessions/backfill = %d %s; want 200 with the six sessions found", rr.Code, rr.Body.String())
	}
	select {
	case <-model.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the route asked for no summary")
	}
	// The start's backlog claims the sixth session while the route's
	// summaries are held.
	go online(t.Context(), []string{"s-6"})
	waitUntil(t, "the backlog to ask for the sixth session's summary", func() bool { return model.count("word6") >= 1 })
	close(model.release)

	waitUntil(t, "every session's summary to complete", func() bool {
		for i := 1; i <= 6; i++ {
			sess, err := store.GetSession(fmt.Sprintf("s-%d", i))
			if err != nil || sess.SummaryStatus != storage.SummaryCompleted {
				return false
			}
		}
		return true
	})
	// The route's last summaries end within milliseconds of the release.
	time.Sleep(300 * time.Millisecond)
	for i := 1; i <= 6; i++ {
		if n := model.count(fmt.Sprintf("word%d", i)); n != 1 {
			t.Errorf("session s-%d was summarized %d times, want once", i, n)
		}
	}
}
