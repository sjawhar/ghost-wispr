package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
)

// onlineSummary is the summary a session's online summary stores in these
// tests (promptRecorder's answer).
const onlineSummary = "The team moved the launch."

// batchClaimUnit is a unit whose summaries go as batch jobs to a fake Gemini
// API (geminiBatches, whose job batches/b-1 succeeds with a summary of
// session "session"), with real storage holding that session, ended.
type batchClaimUnit struct {
	store     *storage.SQLiteStore
	held      *heldConfig
	summaries *summarizers
	hub       *recordingBroadcaster
}

func newBatchClaimUnit(t *testing.T) *batchClaimUnit {
	t.Helper()
	url, _ := geminiBatches(t)
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"gemini":  {API: config.APIGemini, URL: url, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: exampleGeminiKeyEnv}},
			"gateway": {API: config.APIOpenAI, URL: "http://127.0.0.1:1/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_GATEWAY_KEY"}},
		},
		ProviderKeys: map[string]string{exampleGeminiKeyEnv: "gk-1", "EXAMPLE_GATEWAY_KEY": "gw-1"},
		Summarization: config.Summarization{
			Model: "gemini/gemini-x", MinSummaryWords: 1, BatchJobs: true,
			Presets: map[string]config.Preset{"default": {Description: "Summary", UserTemplate: "{{transcript}}"}},
		},
	}
	u := &batchClaimUnit{held: &heldConfig{cfg: cfg}, hub: &recordingBroadcaster{}}
	u.summaries = newSummarizers(summaryClients(u.held), func(error) {})
	if u.summaries.apply(&cfg) == nil {
		t.Fatal("the test config cannot summarize")
	}
	u.store, _, _ = newDeletionTestStore(t, "recording.mp3")
	return u
}

// submit sends the session's summary as batch job batches/b-1.
func (u *batchClaimUnit) submit(t *testing.T) string {
	t.Helper()
	batch, count, err := newSummaryBatchSubmitter(u.store, u.hub, u.summaries, u.held)(t.Context(), []string{"session"})
	if err != nil || batch != "batches/b-1" || count != 1 {
		t.Fatalf("the batch submit = %q, %d sessions, %v; want batches/b-1 holding the session", batch, count, err)
	}
	return batch
}

// restart is what the unit's start does to the summaries the last run left
// running.
func (u *batchClaimUnit) restart(t *testing.T) {
	t.Helper()
	if _, err := u.store.RecoverInterruptedSummaries(); err != nil {
		t.Fatal(err)
	}
}

// summarizeOnline is the start's backlog summarizing the session online.
func (u *batchClaimUnit) summarizeOnline(t *testing.T) storage.Session {
	t.Helper()
	cfg := u.held.Get()
	online := summary.New(cfg.Summarization, func(_, _ string) (llm.Client, error) { return &promptRecorder{}, nil })
	summarizeBackfillSession(context.Background(), u.store, online, u.hub, func(string) {}, "session")
	sess := u.session(t)
	if sess.SummaryStatus != storage.SummaryCompleted || sess.Summary != onlineSummary {
		t.Fatalf("the online summary after the restart: %q (%s); want %q, completed", sess.Summary, sess.SummaryStatus, onlineSummary)
	}
	return sess
}

func (u *batchClaimUnit) poll(t *testing.T, batch string) map[string]any {
	t.Helper()
	result, err := newSummaryBatchPoller(u.store, u.hub, u.summaries, u.held, func(string) {})(t.Context(), batch)
	if err != nil {
		t.Fatalf("poll %s: %v", batch, err)
	}
	return result
}

func (u *batchClaimUnit) session(t *testing.T) storage.Session {
	t.Helper()
	sess, err := u.store.GetSession("session")
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func (u *batchClaimUnit) claimState(t *testing.T, batch string) string {
	t.Helper()
	records, err := u.store.SummaryBatchSessions(batch)
	if err != nil {
		t.Fatal(err)
	}
	return records["session"].State
}

// capturedLog sends the default logger to a buffer for the test.
func capturedLog(t *testing.T) *cliOutput {
	t.Helper()
	var logged cliOutput
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logged
}

// TestAnOldBatchJobLeavesTheSummaryMadeAfterARestart fails if a summary batch
// job that claimed a session before a restart, polled after the start
// summarized the session online, stores its result or its failure over the
// online summary, as the re-proof of a2731a0 found (G1): with its provider
// kept, the job's success replaced the online summary at a new version; with
// its provider removed, giving the job up flipped the good online summary to
// failed. The job's result must be dropped with a log line naming the job and
// the session, and its claim marked discarded. The control: polled after the
// restart with no summary made since, the job's result is still its claim's,
// and is stored.
func TestAnOldBatchJobLeavesTheSummaryMadeAfterARestart(t *testing.T) {
	t.Run("provider kept", func(t *testing.T) {
		logged := capturedLog(t)
		u := newBatchClaimUnit(t)
		batch := u.submit(t)
		u.restart(t)
		online := u.summarizeOnline(t)

		result := u.poll(t, batch)
		if state, _ := result["state"].(string); state != "JOB_STATE_SUCCEEDED" {
			t.Fatalf("the control: the poll read %v; want the job's success", result)
		}
		got := u.session(t)
		if got.Summary != onlineSummary || got.SummaryStatus != storage.SummaryCompleted || got.SummaryVersion != online.SummaryVersion {
			t.Errorf("after the old job's success: summary %q (%s), version %d; want the online summary %q, completed, at version %d", got.Summary, got.SummaryStatus, got.SummaryVersion, onlineSummary, online.SummaryVersion)
		}
		if state := u.claimState(t, batch); state != "discarded" {
			t.Errorf("the old job's claim is %q, want discarded", state)
		}
		if line := logged.String(); !strings.Contains(line, "dropped a stale summary batch result") || !strings.Contains(line, "batch=batches/b-1") || !strings.Contains(line, "session_id=session") {
			t.Errorf("no log line says the job's result was dropped:\n%s", line)
		}
	})

	t.Run("provider removed", func(t *testing.T) {
		logged := capturedLog(t)
		u := newBatchClaimUnit(t)
		batch := u.submit(t)
		u.held.change(func(c *config.Config) {
			c.Summarization.Model = "gateway/chat-example"
			delete(c.Providers, "gemini")
		})
		u.restart(t)
		u.summarizeOnline(t)

		result := u.poll(t, batch)
		if abandoned, _ := result["abandoned"].(bool); !abandoned {
			t.Fatalf("the control: the poll answered %v; want the job given up", result)
		}
		got := u.session(t)
		if got.Summary != onlineSummary || got.SummaryStatus != storage.SummaryCompleted || got.SummaryError != "" {
			t.Errorf("after the old job was given up: summary %q (%s), summary_error %q; want the online summary %q, completed, with no error", got.Summary, got.SummaryStatus, got.SummaryError, onlineSummary)
		}
		if state := u.claimState(t, batch); state != "discarded" {
			t.Errorf("the old job's claim is %q, want discarded", state)
		}
		if line := logged.String(); !strings.Contains(line, "dropped a stale summary batch result") || !strings.Contains(line, "batch=batches/b-1") {
			t.Errorf("no log line says the job's failure was dropped:\n%s", line)
		}
	})

	t.Run("control: nothing made since the restart", func(t *testing.T) {
		u := newBatchClaimUnit(t)
		batch := u.submit(t)
		u.restart(t)

		u.poll(t, batch)
		if got := u.session(t); got.Summary != "The batch summary of the bridge." || got.SummaryStatus != storage.SummaryCompleted {
			t.Errorf("the job's result, with no summary made since it claimed the session: %q (%s); want it stored", got.Summary, got.SummaryStatus)
		}
		if state := u.claimState(t, batch); state != storage.SummaryCompleted {
			t.Errorf("the job's claim is %q, want completed", state)
		}
	})
}

// TestPollingAJobThisUnitNeverSubmittedAnswers404 fails if GET
// /api/batches/<name> for a job this unit has no record of answers other
// than 404, as the re-proof of a2731a0 found (G2): it answered 500 with a
// reason about the current summarization.model. The control: a job the unit
// did submit answers 200, with the summarization model since moved off the
// Gemini API.
func TestPollingAJobThisUnitNeverSubmittedAnswers404(t *testing.T) {
	runParallel(t)
	u := newBatchClaimUnit(t)
	batch := u.submit(t)
	u.held.change(func(c *config.Config) { c.Summarization.Model = "gateway/chat-example" })
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), u.store, &server.ControlHooks{
		BatchPoll: newSummaryBatchPoller(u.store, u.hub, u.summaries, u.held, func(string) {}),
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	get := func(name string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/batches/"+name, nil))
		return rr
	}

	if rr := get(batch); rr.Code != http.StatusOK {
		t.Fatalf("the control, GET /api/batches/%s = %d %s; want 200", batch, rr.Code, rr.Body.String())
	}
	rr := get("batches/job-2")
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /api/batches/batches/job-2, a job this unit never submitted = %d %s; want 404", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); strings.Contains(body, "summarization.model") {
		t.Errorf("the answer about an unknown job speaks of the current model: %s", body)
	}
}
