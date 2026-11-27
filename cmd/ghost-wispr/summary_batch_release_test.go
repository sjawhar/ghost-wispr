package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// backlogTakes fails the test unless POST /api/sessions/backfill, wired as
// main wires it (backlogUntilStop), finds and queues session "session", and
// its online summary completes: no worker and no recorded batch job holds
// the summary.
func backlogTakes(t *testing.T, store *storage.SQLiteStore, status string) {
	t.Helper()
	later := backlogUntilStop(stopTestManager(t, store), newOnlineSummaries(store, fixedSummarizerOf(fixedSummarizer{}), &recordingBroadcaster{}, func(string) {}))
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, &server.ControlHooks{SummarizeBacklog: later}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/backfill", nil))
	var answer struct{ Found, Queued int }
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &answer) != nil || answer.Found != 1 || answer.Queued != 1 {
		t.Fatalf("after the batch submit the summary is %s; POST /api/sessions/backfill = %d %s; want the session found and queued: no worker and no recorded batch job holds it", status, rr.Code, rr.Body.String())
	}
	waitUntil(t, "the backlog to make the summary", func() bool {
		got := refinementOf(t, store, "session")
		return got.SummaryStatus == storage.SummaryCompleted && got.Summary == "A summary."
	})
}

// TestAnUnsubmittedBatchClaimWhoseFailureWasNotStoredIsTakenAgainInThisRun
// fails if a batch submit the provider refused (GPT-6.1 Sol's probe at
// 1db0cbc), whose failure write the store also refused, leaves the session's
// summary running with no worker and no batch job on it: a backlog of this
// run then finds nothing until a restart. Its control: the same refusal with
// the failure stored.
func TestAnUnsubmittedBatchClaimWhoseFailureWasNotStoredIsTakenAgainInThisRun(t *testing.T) {
	for _, fault := range []bool{false, true} {
		name := "control: the failure is stored"
		if fault {
			name = "the failure write is refused"
		}
		t.Run(name, func(t *testing.T) {
			provider, held, summaries, store := batchUnit(t, config.APIGemini)
			remove := func() {}
			if fault {
				remove = failSummaryEnds(t, store)
			}
			submit := newSummaryBatchSubmitter(store, &recordingBroadcaster{}, summaries, held)
			if batch, _, err := submit(t.Context(), []string{"session"}); err == nil {
				t.Fatalf("control: the refusing provider took batch job %q", batch)
			}
			remove()
			if calls := provider.seen(); len(calls) != 1 || calls[0] != "batch" {
				t.Fatalf("control: the provider saw %v; want one batch create", calls)
			}
			backlogTakes(t, store, refinementOf(t, store, "session").SummaryStatus)
		})
	}
}

// TestABatchJobThatWasNotRecordedIsTakenAgainInThisRun fails if a batch job
// the provider took, but whose record the store refused (so no poll can store
// its results), leaves the session's summary running when its failure write
// is refused too.
func TestABatchJobThatWasNotRecordedIsTakenAgainInThisRun(t *testing.T) {
	runParallel(t)
	u := newBatchClaimUnit(t)
	if _, err := u.store.DB().Exec(`CREATE TEMP TRIGGER test_refuse_batch_record BEFORE INSERT ON summary_batches
		BEGIN SELECT RAISE(ABORT, 'planted: the store refused the batch record'); END`); err != nil {
		t.Fatal(err)
	}
	remove := failSummaryEnds(t, u.store)
	if batch, _, err := newSummaryBatchSubmitter(u.store, u.hub, u.summaries, u.held)(t.Context(), []string{"session"}); err == nil {
		t.Fatalf("control: batch job %q was recorded despite the refusal", batch)
	}
	remove()
	if _, err := u.store.DB().Exec(`DROP TRIGGER test_refuse_batch_record`); err != nil {
		t.Fatal(err)
	}
	backlogTakes(t, u.store, refinementOf(t, u.store, "session").SummaryStatus)
}

// TestARecordedBatchJobKeepsItsClaim fails if the submit releases a claim
// whose batch job the provider took and the store recorded: the job's poll
// stores its result, and a backlog must not summarize the session again in
// the meantime.
func TestARecordedBatchJobKeepsItsClaim(t *testing.T) {
	runParallel(t)
	u := newBatchClaimUnit(t)
	u.submit(t)
	if got := refinementOf(t, u.store, "session"); got.SummaryStatus != storage.SummaryRunning {
		t.Fatalf("after a recorded batch submit the summary is %s; want it running, held by the job", got.SummaryStatus)
	}
	ids, err := u.store.GetSessionsForBackfill()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("a backlog takes %v while a recorded batch job holds the summary; want none", ids)
	}
}
