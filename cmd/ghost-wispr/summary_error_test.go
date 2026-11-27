package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// failingReportedSummarizer fails every summary with err, as the summarizer
// does once its retries are spent.
type failingReportedSummarizer struct{ err error }

func (f failingReportedSummarizer) SummarizeReported(context.Context, string, string) (string, string, string, string, string, error) {
	return "", "", "", "", "", f.err
}

// TestABackfilledSummaryThatFailsSaysWhy fails if a summary made online by
// POST /api/sessions/batch-backfill or the start's backlog
// (summarizeBackfillSession), failing after its retries, leaves no reason in
// the session's summary_error as GET /api/sessions/{id} answers it, or takes
// away the session's last good summary. The session is the one the
// re-proof of 2026-11-20 planted: a last good summary, its summary failed.
func TestABackfilledSummaryThatFailsSaysWhy(t *testing.T) {
	runParallel(t)
	store := summaryStore(t, "We agreed on the launch date and the docking fix for the warehouse robot this week.")
	if _, err := store.DB().Exec(`UPDATE sessions SET title = 'Planted', summary = 'Last good summary.', summary_status = ? WHERE id = ?`, storage.SummaryFailed, summarySessionID); err != nil {
		t.Fatal(err)
	}
	cause := fmt.Errorf("summarize failed after retries: %w", errors.New("the model's answer begins as a JSON object but does not parse"))

	summarizeBackfillSession(context.Background(), store, failingReportedSummarizer{err: cause}, &recordingBroadcaster{}, func(string) {}, summarySessionID)

	rr := httptest.NewRecorder()
	searchHandler(t, store, nil).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/"+summarySessionID, nil))
	var answer struct {
		Session struct {
			Summary       string `json:"summary"`
			SummaryStatus string `json:"summary_status"`
			SummaryError  string `json:"summary_error"`
		} `json:"session"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &answer) != nil {
		t.Fatalf("GET /api/sessions/%s = %d %s", summarySessionID, rr.Code, rr.Body.String())
	}
	got := answer.Session
	if want := "summarization failed: " + cause.Error(); got.SummaryError != want {
		t.Errorf("summary_error = %q, want %q", got.SummaryError, want)
	}
	if got.SummaryStatus != storage.SummaryFailed || got.Summary != "Last good summary." {
		t.Errorf("summary_status %q, summary %q; want failed, keeping the last good summary", got.SummaryStatus, got.Summary)
	}
}
