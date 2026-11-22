package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// heldSummaries is a Vertex AI summarization provider that answers every
// summary, the first only once release is closed. It counts the summaries
// asked of each session, which it tells by a word of its transcript.
type heldSummaries struct {
	release chan struct{}
	asked   chan string // each session's word, as its summary is asked
	mu      sync.Mutex
	counts  map[string]int
	first   bool
}

func (h *heldSummaries) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !strings.HasSuffix(r.URL.Path, ":generateContent") {
		http.NotFound(w, r)
		return
	}
	word := ""
	for _, w := range []string{"alphaword", "bravoword"} {
		if strings.Contains(string(body), w) {
			word = w
		}
	}
	h.mu.Lock()
	h.counts[word]++
	first := !h.first
	h.first = true
	h.mu.Unlock()
	h.asked <- word
	if first {
		<-h.release
	}
	answer, _ := json.Marshal(map[string]string{"title": "Bridge talk", "summary": "Summary of " + word})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"candidates":   []map[string]any{{"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": string(answer)}}}}},
		"modelVersion": "gemini-x",
	})
}

func (h *heldSummaries) count(word string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[word]
}

// TestABackfillDuringTheStartsBacklogSummarizesEachSessionOnce fails if POST
// /api/sessions/batch-backfill, sent while the start's backlog summarizes
// its first session, takes that session again, as it did on 2026-11-20 (one
// session summarized twice, both results stored), or if a session the
// route took is summarized again when the start's backlog reaches it: each
// session's summary is asked once. The start still takes a summary the last
// run left running (storage's RecoverInterruptedSummaries makes it pending).
func TestABackfillDuringTheStartsBacklogSummarizesEachSessionOnce(t *testing.T) {
	provider := &heldSummaries{release: make(chan struct{}), asked: make(chan string, 16), counts: map[string]int{}}
	srv := httptest.NewServer(provider)
	t.Cleanup(srv.Close)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(provider.release) }) }
	t.Cleanup(release) // runs before srv.Close, which waits for the held call
	cfg := vertexTestConfig(t, srv.URL, vertexTestToken)
	cfg.Summarization = config.Summarization{
		Model: "vertex/gemini-x", MinSummaryWords: 1,
		Presets: map[string]config.Preset{"default": {Description: "Summary", UserTemplate: "{{transcript}}"}},
	}
	held := &heldConfig{cfg: *cfg}
	summaries := newSummarizers(summaryClients(held), func(error) {})
	if summaries.apply(cfg) == nil {
		t.Fatal("the test config cannot summarize")
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "backfill.db"))
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
		if err := store.AppendSegment(id, transcribe.Segment{Text: "We talked about the " + word + " bridge.", StartTime: 0, EndTime: 2, Timestamp: at}); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	// The first session's summary was running when the last run stopped.
	if _, err := store.StartSummary("s-alphaword"); err != nil {
		t.Fatal(err)
	}

	// The start, as main runs it.
	if _, err := store.RecoverInterruptedSummaries(); err != nil {
		t.Fatal(err)
	}
	ids, err := store.GetSessionsForBackfill()
	if err != nil || len(ids) != 2 {
		t.Fatalf("the start's backlog = %v, %v; want both sessions, the one left running included", ids, err)
	}
	online := newOnlineSummaries(store, summaries.summarizer, &recordingBroadcaster{}, func(string) {})
	manager := stopTestManager(t, store)
	startDone, routeDone := make(chan struct{}), make(chan struct{})
	startSummaryBackfill(t.Context(), manager, summaryBackfill{store: store, online: func(ctx context.Context, ids []string) {
		online(ctx, ids)
		close(startDone)
	}})
	select {
	case word := <-provider.asked:
		if word != "alphaword" {
			t.Fatalf("the start's backlog asked first for %q, want alphaword", word)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the start's backlog asked for no summary")
	}

	// The route, while the start's first summary is out.
	backfill := newBatchBackfill(store, summaries.summarizer, held, nil, backlogUntilStop(manager, func(ctx context.Context, ids []string) {
		online(ctx, ids)
		close(routeDone)
	}))
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store,
		&server.ControlHooks{BatchBackfill: backfill}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/batch-backfill", nil))
	var answer struct {
		Sessions int `json:"sessions"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &answer) != nil || answer.Sessions != 1 {
		t.Errorf("POST /api/sessions/batch-backfill during the start's backlog = %d %s; want 200 and 1 session, the one not being summarized", rr.Code, rr.Body.String())
	}
	release()
	for _, done := range []chan struct{}{startDone, routeDone} {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a backlog did not finish")
		}
	}
	for _, word := range []string{"alphaword", "bravoword"} {
		if n := provider.count(word); n != 1 {
			t.Errorf("session s-%s was summarized %d times, want once", word, n)
		}
		sess, err := store.GetSession("s-" + word)
		if err != nil || sess.SummaryStatus != storage.SummaryCompleted || sess.Summary != "Summary of "+word {
			t.Errorf("session s-%s: summary %q (%s), %v; want completed with its summary", word, sess.Summary, sess.SummaryStatus, err)
		}
	}
}
