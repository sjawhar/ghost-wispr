package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// failSummaryEnds plants a fault in store: every write that ends a summary,
// with its text (completed) or its failure (failed), is refused, as a full
// disk would refuse it. The returned func removes the fault.
func failSummaryEnds(t *testing.T, store *storage.SQLiteStore) (remove func()) {
	t.Helper()
	if _, err := store.DB().Exec(`CREATE TEMP TRIGGER test_refuse_summary_end BEFORE UPDATE OF summary_status ON sessions
		WHEN NEW.summary_status IN ('completed', 'failed') BEGIN SELECT RAISE(ABORT, 'planted: the store refused the summary''s end'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := store.DB().Exec(`DROP TRIGGER test_refuse_summary_end`); err != nil {
			t.Fatal(err)
		}
	}
}

// panickingSummarizer panics in every call, as a fault in a summarizer would.
type panickingSummarizer struct{}

func (panickingSummarizer) Summarize(context.Context, string, string) (string, string, string, string, error) {
	panic("planted: the summarizer panicked")
}

func (panickingSummarizer) SummarizeWithPreset(context.Context, string, string, string) (string, string, string, error) {
	panic("planted: the summarizer panicked")
}

func (panickingSummarizer) SummarizeReported(context.Context, string, string) (string, string, string, string, string, error) {
	panic("planted: the summarizer panicked")
}

// TestASummaryWhoseEndWasNotStoredIsTakenAgainInThisRun fails if a summary
// worker (the backlog's, a requested one, a merged session's) that stored
// neither its summary nor its failure (the store refused both writes, or the
// worker panicked) leaves the session's summary running with no worker on
// it. A backlog of this run then finds nothing (GPT-6.1 Sol's probe at
// f3e2642: POST /api/sessions/backfill answered {"found":0,"queued":0}) until
// a restart recovers it. The summary must go back to pending, and the next
// backlog, wired as main wires it, make it.
func TestASummaryWhoseEndWasNotStoredIsTakenAgainInThisRun(t *testing.T) {
	const id = "20261122090000"
	hub, publish := &recordingBroadcaster{}, func(string) {}
	for _, tc := range []struct {
		name  string
		fault bool
		run   func(store *storage.SQLiteStore)
	}{
		{"the backlog's, its end refused", true, func(store *storage.SQLiteStore) {
			summarizeBackfillSession(t.Context(), store, fixedSummarizer{}, hub, publish, id)
		}},
		{"a requested one, its end refused", true, func(store *storage.SQLiteStore) {
			_ = resummarize(t.Context(), store, fixedSummarizer{}, hub, publish, id, "")
		}},
		{"a merged session's, its end refused", true, func(store *storage.SQLiteStore) {
			summarizeMergedSession(t.Context(), store, fixedSummarizer{}, hub, publish, id)
		}},
		{"the backlog's, panicked", false, func(store *storage.SQLiteStore) {
			defer func() { _ = recover() }()
			summarizeBackfillSession(t.Context(), store, panickingSummarizer{}, hub, publish, id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openResumeStore(t, filepath.Join(t.TempDir(), "release.db"))
			endedSessionWithWords(t, store, id, storage.SummaryFailed)
			remove := func() {}
			if tc.fault {
				remove = failSummaryEnds(t, store)
			}
			tc.run(store)
			remove()
			if got := refinementOf(t, store, id); got.SummaryStatus != storage.SummaryPending {
				t.Fatalf("after a summary whose end was not stored, the session's summary is %s; want it pending, for a backlog of this run", got.SummaryStatus)
			}

			// The next backlog, as main wires POST /api/sessions/backfill.
			later := backlogUntilStop(stopTestManager(t, store), newOnlineSummaries(store, fixedSummarizerOf(fixedSummarizer{}), hub, publish))
			h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, &server.ControlHooks{SummarizeBacklog: later}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/backfill", nil))
			var answer struct{ Found, Queued int }
			if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &answer) != nil || answer.Found != 1 || answer.Queued != 1 {
				t.Fatalf("POST /api/sessions/backfill = %d %s; want the session found and queued", rr.Code, rr.Body.String())
			}
			waitUntil(t, "the backlog to make the summary", func() bool {
				got := refinementOf(t, store, id)
				return got.SummaryStatus == storage.SummaryCompleted && got.Summary == "A summary."
			})
		})
	}
}
