package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// geminiBatches is the Gemini API's batch endpoints as a test sees them: a
// batchGenerateContent takes the job as batches/b-1, and batches.get answers
// it succeeded, with the summary of session "session". It counts the gets.
func geminiBatches(t *testing.T) (url string, gets *atomic.Int32) {
	t.Helper()
	gets = &atomic.Int32{}
	answer := `{"title":"Bridge talk","summary":"The batch summary of the bridge.","speakers":[]}`
	quoted, _ := json.Marshal(answer)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":batchGenerateContent"):
			_, _ = io.WriteString(w, `{"name":"batches/b-1","metadata":{"name":"batches/b-1","state":"BATCH_STATE_PENDING"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/batches/b-1"):
			gets.Add(1)
			_, _ = fmt.Fprintf(w, `{"name":"batches/b-1","metadata":{"name":"batches/b-1","state":"BATCH_STATE_SUCCEEDED","output":{"inlinedResponses":{"inlinedResponses":[{"metadata":{"session_id":"session","preset":"default"},"response":{"candidates":[{"content":{"role":"model","parts":[{"text":%s}]}}]}}]}}}}`, quoted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, gets
}

// TestABatchJobIsPolledWithTheProviderThatTookIt fails if a summary batch
// job is polled with the provider of the summarization.model saved last
// rather than the one that took it, as GPT-6.1 Sol's review of 4e85fcf
// found: after summaries moved from the Gemini API to another provider, the
// poll was refused (summary batch jobs run on the Gemini API only) and the
// sessions the job claimed stayed running, which no backlog takes. A job
// whose provider still signs in must be polled to its end; once its provider
// is gone, or cannot sign in, its sessions must fail with the reason and go
// back to the backlog, and the start's poll of it must stop.
func TestABatchJobIsPolledWithTheProviderThatTookIt(t *testing.T) {
	runParallel(t)
	for _, tc := range []struct {
		name   string
		change func(c *config.Config)
		polled bool   // the job is polled to its end
		reason string // why the sessions fail when it is not
	}{
		{"summaries moved to another provider", func(c *config.Config) { c.Summarization.Model = "gateway/chat-example" }, true, ""},
		{"its provider removed", func(c *config.Config) {
			c.Summarization.Model = "gateway/chat-example"
			delete(c.Providers, "gemini")
		}, false, "the batch job's provider gemini"},
		{"its provider has no key", func(c *config.Config) {
			c.Summarization.Model = "gateway/chat-example"
			delete(c.ProviderKeys, exampleGeminiKeyEnv)
		}, false, "the batch job's provider gemini"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, gets := geminiBatches(t)
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
			held := &heldConfig{cfg: cfg}
			summaries := newSummarizers(summaryClients(held), func(error) {})
			if summaries.apply(&cfg) == nil {
				t.Fatal("the test config cannot summarize")
			}
			store, _, _ := newDeletionTestStore(t, "recording.mp3")
			hub := &recordingBroadcaster{}
			batch, count, err := newSummaryBatchSubmitter(store, hub, summaries, held)(t.Context(), []string{"session"})
			if err != nil || batch != "batches/b-1" || count != 1 {
				t.Fatalf("the batch submit = %q, %d sessions, %v; want batches/b-1 holding the session", batch, count, err)
			}

			held.change(tc.change)
			changed := held.Get()
			summaries.apply(&changed)
			poll := newSummaryBatchPoller(store, hub, summaries, held, func(string) {})
			result, err := poll(t.Context(), batch)
			if err != nil {
				t.Fatalf("the poll after the settings change: %v", err)
			}
			sess, err := store.GetSession("session")
			if err != nil {
				t.Fatal(err)
			}
			backlog, err := store.GetSessionsForBackfill()
			if err != nil {
				t.Fatal(err)
			}
			if tc.polled {
				if gets.Load() == 0 || sess.SummaryStatus != storage.SummaryCompleted || sess.Summary != "The batch summary of the bridge." {
					t.Fatalf("after %s the poll asked the job's provider %d times; the session's summary is %q (%s); want the job polled to its end and its summary stored", tc.name, gets.Load(), sess.Summary, sess.SummaryStatus)
				}
				return
			}
			if gets.Load() != 0 {
				t.Errorf("the poll asked a provider the config no longer signs in to %d times", gets.Load())
			}
			if sess.SummaryStatus != storage.SummaryFailed || !strings.Contains(sess.SummaryError, tc.reason) {
				t.Errorf("after %s: summary_status %q, summary_error %q; want failed, saying %q", tc.name, sess.SummaryStatus, sess.SummaryError, tc.reason)
			}
			if !slices.Contains(backlog, "session") {
				t.Errorf("the backlog %q does not hold the session", backlog)
			}
			if reason, _ := result["error"].(string); !strings.Contains(reason, tc.reason) {
				t.Errorf("the poll's answer %v; want its error saying %q", result, tc.reason)
			}
			stopped := make(chan struct{})
			go func() {
				pollStartupSummaryBatch(t.Context(), poll, batch, 10*time.Millisecond)
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Errorf("the start's poll of a job whose provider is gone did not stop")
			}
		})
	}
}
