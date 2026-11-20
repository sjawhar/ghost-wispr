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
// (online, with ctx). It returns the batch job's name, "" when the backlog
// went online, and the sessions taken. A batch job that is not taken goes
// online too, with a log line saying why: a provider that takes none
// (errNoBatchProvider, such as Vertex AI) is no fault; a refusal is a
// warning.
func summarizeBacklog(ctx context.Context, batchJobs bool, ids []string, minBatch int,
	submit func(context.Context, []string) (string, int, error), online func(context.Context, []string),
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
	online(ctx, ids)
	return "", len(ids)
}

// newOnlineSummaries summarizes each session of a backlog online, in order,
// with the summarizer current returns at each (summarizers.summarizer), until
// it returns none (summaries turned off) or ctx ends, announcing each on hub.
func newOnlineSummaries(store *storage.SQLiteStore, current func() unitSummarizer, hub summaryBroadcaster, publish func(sessionID string)) func(ctx context.Context, ids []string) {
	return func(ctx context.Context, ids []string) {
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			summarizer := current()
			if summarizer == nil {
				log.Printf("summary backlog: summaries were turned off; the sessions left keep their summaries as they are")
				return
			}
			summarizeBackfillSession(ctx, store, summarizer, hub, publish, id)
		}
	}
}

// backlogUntilStop summarizes a backlog's sessions online (online) in the
// background, as work the stop cuts off and waits for (backgroundWork.Go): a
// backlog outlives the request that asked for it. A summary the stop cuts off
// is left running, for the next start's backlog.
func backlogUntilStop(background backgroundWork, online func(context.Context, []string)) func(ids []string) {
	return func(ids []string) {
		background.Go(func(ctx context.Context) { online(ctx, ids) })
	}
}

// newBatchBackfill is POST /api/sessions/batch-backfill: every session whose
// summary is missing, failed or never ended, as one batch job (submit) when
// summarization.batch_jobs is on, else each online in the background (later,
// backlogUntilStop). With summaries off (current returns none) it takes
// none.
func newBatchBackfill(store *storage.SQLiteStore, current func() unitSummarizer, cfgStore interface{ Get() config.Config },
	submit func(context.Context, []string) (string, int, error), later func([]string),
) func(context.Context) (string, int, error) {
	return func(reqCtx context.Context) (string, int, error) {
		sessionIDs, err := store.GetSessionsForBackfill()
		if err != nil {
			return "", 0, fmt.Errorf("query sessions: %w", err)
		}
		if len(sessionIDs) == 0 {
			return "", 0, nil
		}
		if current() == nil {
			return "", 0, errSummariesOff
		}
		batch, count := summarizeBacklog(reqCtx, cfgStore.Get().Summarization.BatchJobs, sessionIDs, 1, submit, func(_ context.Context, ids []string) { later(ids) })
		return batch, count, nil
	}
}
