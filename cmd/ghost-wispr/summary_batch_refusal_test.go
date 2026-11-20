package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// batchRefuser is a summarization provider that refuses every batch job
// create, as the office Pi's project does, and answers each online summary:
// the Gemini API's batchGenerateContent and Vertex AI's batchPredictionJobs
// are refused, generateContent answers.
type batchRefuser struct {
	mu    sync.Mutex
	calls []string // "batch" for a batch job create, "online" for a summary
}

func (f *batchRefuser) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, ":batchGenerateContent"), strings.HasSuffix(r.URL.Path, "/batchPredictionJobs"):
			f.record("batch")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"Permission denied on batch jobs","status":"PERMISSION_DENIED"}}`)
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			f.record("online")
			answer, _ := json.Marshal(map[string]string{"title": "Bridge talk", "summary": "Online summary of the bridge"})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"candidates":   []map[string]any{{"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": string(answer)}}}}},
				"modelVersion": "gemini-x",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *batchRefuser) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *batchRefuser) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// batchUnit is a unit whose summaries run on a provider of api (the Gemini
// API or Vertex AI) that refuses batch job creates, with
// summarization.batch_jobs on, and one ended session whose summary is
// pending.
func batchUnit(t *testing.T, api string) (*batchRefuser, *heldConfig, *summarizers, *storage.SQLiteStore) {
	t.Helper()
	provider := &batchRefuser{}
	url := provider.server(t).URL
	var cfg *config.Config
	switch api {
	case config.APIVertex:
		cfg = vertexTestConfig(t, url, vertexTestToken)
	case config.APIGemini:
		key := geminiKeyConfig("gk-1")
		cfg = &key
		p := cfg.Providers["gemini"]
		p.URL = url
		cfg.Providers["gemini"] = p
	}
	model := map[string]string{config.APIVertex: "vertex/gemini-x", config.APIGemini: "gemini/gemini-x"}[api]
	cfg.Summarization = config.Summarization{
		Model: model, MinSummaryWords: 1, BatchJobs: true,
		Presets: map[string]config.Preset{"default": {Description: "Summary", UserTemplate: "{{transcript}}"}},
	}
	held := &heldConfig{cfg: *cfg}
	summaries := newSummarizers(summaryClients(held), func(error) {})
	if summaries.apply(cfg) == nil {
		t.Fatal("the test config cannot summarize")
	}
	store, _, _ := newDeletionTestStore(t, "recording.mp3")
	return provider, held, summaries, store
}

// postBatchBackfill sends POST /api/sessions/batch-backfill to a server with
// the unit's backfill, and waits for the session's summary to complete.
func postBatchBackfill(t *testing.T, held *heldConfig, summaries *summarizers, store *storage.SQLiteStore) storage.Session {
	t.Helper()
	hub := &recordingBroadcaster{}
	backfill := newBatchBackfill(store, summaries, held,
		newSummaryBatchSubmitter(store, hub, summaries, held),
		newOnlineSummaries(t.Context(), store, summaries, hub, func(string) {}))
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store,
		&server.ControlHooks{BatchBackfill: backfill}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/batch-backfill", nil))
	var answer struct {
		BatchJob *string `json:"batch_job"`
		Sessions int     `json:"sessions"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &answer) != nil || answer.BatchJob == nil || *answer.BatchJob != "" || answer.Sessions != 1 {
		t.Fatalf("POST /api/sessions/batch-backfill = %d %s; want 200 {\"batch_job\":\"\",\"sessions\":1}", rr.Code, rr.Body.String())
	}
	var sess storage.Session
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if sess, err = store.GetSession("session"); err == nil && sess.SummaryStatus == storage.SummaryCompleted {
			break
		}
	}
	if sess.SummaryStatus != storage.SummaryCompleted || sess.Summary != "Online summary of the bridge" {
		t.Fatalf("after POST /api/sessions/batch-backfill the session's summary is %q (%s), want the online summary", sess.Summary, sess.SummaryStatus)
	}
	return sess
}

// TestARefusedSummaryBatchLeavesNoSessionRunning fails if a summary batch job
// the provider refuses leaves its sessions at summary_status running, which
// nothing ends: the page would show them summarizing for ever.
func TestARefusedSummaryBatchLeavesNoSessionRunning(t *testing.T) {
	provider, held, summaries, store := batchUnit(t, config.APIGemini)
	submit := newSummaryBatchSubmitter(store, &recordingBroadcaster{}, summaries, held)
	if name, _, err := submit(t.Context(), []string{"session"}); err == nil {
		t.Fatalf("a batch job the provider refused was taken as %q", name)
	}
	if got := provider.seen(); !slices.Equal(got, []string{"batch"}) {
		t.Fatalf("the provider was asked %q, want one batch job create", got)
	}
	sess, err := store.GetSession("session")
	if err != nil {
		t.Fatal(err)
	}
	if sess.SummaryStatus != storage.SummaryFailed {
		t.Fatalf("after the refused batch job the session's summary_status is %q, want %q", sess.SummaryStatus, storage.SummaryFailed)
	}
}

// TestABatchBackfillTheProviderRefusesGoesOnline fails if POST
// /api/sessions/batch-backfill, when the provider refuses the batch job,
// answers anything but 200 {"batch_job":"","sessions":N}, or does not
// summarize each session online instead, as the start's backlog does.
func TestABatchBackfillTheProviderRefusesGoesOnline(t *testing.T) {
	provider, held, summaries, store := batchUnit(t, config.APIGemini)
	postBatchBackfill(t, held, summaries, store)
	if got := provider.seen(); !slices.Equal(got, []string{"batch", "online"}) {
		t.Fatalf("the provider was asked %q, want a batch job create, then an online summary", got)
	}
}

// TestAVertexAISummaryBacklogGoesOnline fails if a backlog on a Vertex AI
// summarization provider is sent as a batch job (genai sends Vertex AI no
// inlined batch request, so it never worked), leaves a session running, is
// not summarized online, or goes online without a log line that says why.
func TestAVertexAISummaryBacklogGoesOnline(t *testing.T) {
	keepDefaultLogger(t)
	// The online summaries log from their own goroutine while the test reads.
	var logged cliOutput
	log.SetOutput(&logged)
	provider, held, summaries, store := batchUnit(t, config.APIVertex)
	postBatchBackfill(t, held, summaries, store)
	if got := provider.seen(); !slices.Equal(got, []string{"online"}) {
		t.Fatalf("the provider was asked %q, want only an online summary", got)
	}
	if got := logged.String(); !strings.Contains(got, "summary batch jobs run on the Gemini API only") {
		t.Fatalf("logged %q, want a line saying the backlog goes online because batch jobs run on the Gemini API only", got)
	}
}

// TestAnOnlineSummaryLogSaysOnline fails if a session summarized online, at
// the start or on POST /api/sessions/batch-backfill, is logged as a start's
// synchronous summary.
func TestAnOnlineSummaryLogSaysOnline(t *testing.T) {
	keepDefaultLogger(t)
	var logged bytes.Buffer
	log.SetOutput(&logged)
	store, _, _ := newDeletionTestStore(t, "recording.mp3")
	summarizeBackfillSession(context.Background(), store, &racingSummarizer{}, &recordingBroadcaster{}, func(string) {}, "session")
	if got := logged.String(); !strings.Contains(got, "summarizing session session online") || strings.Contains(got, "startup:") {
		t.Fatalf("logged %q, want \"summarizing session session online\" and no startup prefix", got)
	}
}
