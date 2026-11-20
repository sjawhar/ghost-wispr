package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// startupBatchMin is the fewest sessions a start's summary backlog sends as
// one batch job; a smaller backlog is summarized online.
const startupBatchMin = 4

// summarizeBacklog summarizes a backlog of sessions: as one batch job of the
// summarization provider (submit) when batchJobs (summarization.batch_jobs)
// is on and the backlog holds at least minBatch sessions, else each online
// (online, started in its own goroutine, which keeps the unit's own context).
// It returns the batch job's name, "" when the backlog goes online, and the
// sessions taken. A batch job that is not taken goes online too, with a log
// line saying why: a provider that takes none (errNoBatchProvider, such as
// Vertex AI) is no fault; a refusal is a warning.
func summarizeBacklog(ctx context.Context, batchJobs bool, ids []string, minBatch int,
	submit func(context.Context, []string) (string, int, error), online func([]string),
) (batch string, count int) {
	if batchJobs && len(ids) >= minBatch {
		batch, count, err := submit(ctx, ids)
		switch {
		case err == nil:
			return batch, count
		case errors.Is(err, errNoBatchProvider):
			log.Printf("summary backlog: summarizing %d sessions online: %v", len(ids), err)
		default:
			slog.Warn("summary batch job not taken; summarizing each session online", "sessions", len(ids), "error", err)
		}
	}
	go online(ids)
	return "", len(ids)
}

// newOnlineSummaries summarizes each session of a backlog online, in order,
// with the summarizer of the settings saved last, until summaries are turned
// off, announcing each on hub. ctx is the unit's: a backlog outlives the
// request that started it.
func newOnlineSummaries(ctx context.Context, store *storage.SQLiteStore, summaries *summarizers, hub summaryBroadcaster, publish func(sessionID string)) func(ids []string) {
	return func(ids []string) {
		for _, id := range ids {
			current := summaries.get()
			if current == nil {
				log.Printf("summary backlog: summaries were turned off; the sessions left keep their summaries as they are")
				return
			}
			summarizeBackfillSession(ctx, store, current, hub, publish, id)
		}
	}
}

// newBatchBackfill is POST /api/sessions/batch-backfill: every session whose
// summary is missing, failed or never ended, as one batch job (submit) when
// summarization.batch_jobs is on, else each online (online).
func newBatchBackfill(store *storage.SQLiteStore, summaries *summarizers, cfgStore interface{ Get() config.Config },
	submit func(context.Context, []string) (string, int, error), online func([]string),
) func(context.Context) (string, int, error) {
	return func(reqCtx context.Context) (string, int, error) {
		sessionIDs, err := store.GetSessionsForBackfill()
		if err != nil {
			return "", 0, fmt.Errorf("query sessions: %w", err)
		}
		if len(sessionIDs) == 0 {
			return "", 0, nil
		}
		if summaries.get() == nil {
			return "", 0, errSummariesOff
		}
		batch, count := summarizeBacklog(reqCtx, cfgStore.Get().Summarization.BatchJobs, sessionIDs, 1, submit, online)
		return batch, count, nil
	}
}
