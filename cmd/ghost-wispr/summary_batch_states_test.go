package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestABatchJobThatEndsUnsuccessfullyFailsItsSessions fails if a summary
// batch job that ends failed, cancelled or expired, read through the real
// genai client from the Gemini API's batches.get, leaves the sessions it
// claimed running: a running summary is not backfillable, so nothing would
// summarize them until a restart. Each must fail at the generation its job
// read, with the job's end as the reason, announced to the page; its batch
// record must be done; the session must be backfillable again; and the
// start's poll of the job must stop.
func TestABatchJobThatEndsUnsuccessfullyFailsItsSessions(t *testing.T) {
	runParallel(t)
	const batchName = "batches/b-1"
	for _, tc := range []struct {
		batchState string
		jobState   genai.JobState
		reason     string
	}{
		{"BATCH_STATE_FAILED", genai.JobStateFailed, "the batch job failed"},
		{"BATCH_STATE_CANCELLED", genai.JobStateCancelled, "the batch job was cancelled"},
		{"BATCH_STATE_EXPIRED", genai.JobStateExpired, "the batch job expired"},
	} {
		t.Run(tc.batchState, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/"+batchName) {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"name":%q,"metadata":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1main.GenerateContentBatch","name":%q,"state":%q}}`, batchName, batchName, tc.batchState)
			}))
			t.Cleanup(srv.Close)
			cfg := geminiKeyConfig("gk-1")
			p := cfg.Providers["gemini"]
			p.URL = srv.URL
			cfg.Providers["gemini"] = p
			cfg.Summarization = config.Summarization{
				Model: "gemini/gemini-x", MinSummaryWords: 1, BatchJobs: true,
				Presets: map[string]config.Preset{"default": {Description: "Summary", UserTemplate: "{{transcript}}"}},
			}
			held := &heldConfig{cfg: cfg}
			summaries := newSummarizers(summaryClients(held), func(error) {})
			if summaries.apply(&cfg) == nil {
				t.Fatal("the test config cannot summarize")
			}
			store, _, _ := newDeletionTestStore(t, "recording.mp3")
			// The session the job holds, claimed and recorded as the batch
			// submit claims and records it.
			_, generation, err := store.Canonicalize("session")
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := store.StartBatchSummary("session")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordSummaryBatch(batchName, "gemini", map[string]storage.SummaryClaim{"session": {Generation: generation, Attempt: attempt}}); err != nil {
				t.Fatal(err)
			}
			hub := &recordingBroadcaster{}
			poll := newSummaryBatchPoller(store, hub, summaries, held, func(string) {})

			result, err := poll(t.Context(), batchName)
			if err != nil {
				t.Fatalf("poll: %v", err)
			}
			// The control: the poll read the state the fake answered.
			if state, _ := result["state"].(string); state != string(tc.jobState) {
				t.Fatalf("the poll read state %q, want %q: the fake's state was not read", state, tc.jobState)
			}
			sess, err := store.GetSession("session")
			if err != nil {
				t.Fatal(err)
			}
			if sess.SummaryStatus != storage.SummaryFailed || !strings.Contains(sess.SummaryError, tc.reason) {
				t.Errorf("after a job that ended %s: summary_status %q, summary_error %q; want failed, saying %q", tc.jobState, sess.SummaryStatus, sess.SummaryError, tc.reason)
			}
			records, err := store.SummaryBatchSessions(batchName)
			if err != nil {
				t.Fatal(err)
			}
			if state := records["session"].State; state != storage.SummaryFailed {
				t.Errorf("the job's record of the session is %q, want %q", state, storage.SummaryFailed)
			}
			backlog, err := store.GetSessionsForBackfill()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(backlog, "session") {
				t.Errorf("the backlog %q does not hold the session: nothing would summarize it until a restart", backlog)
			}
			hub.mu.Lock()
			announced := slices.Clone(hub.events)
			hub.mu.Unlock()
			if len(announced) == 0 || announced[len(announced)-1].status != storage.SummaryFailed {
				t.Errorf("announced %+v; want the failed summary last", announced)
			}

			stopped := make(chan struct{})
			go func() {
				pollStartupSummaryBatch(t.Context(), poll, batchName, 10*time.Millisecond)
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Errorf("the start's poll of a job that ended %s did not stop", tc.jobState)
			}
		})
	}
}
