package main

import (
	"context"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// storeWork is what the API's requests need for their work on the store.
type storeWork struct {
	manager  *session.Manager
	store    *storage.SQLiteStore
	drive    *driveSync
	deletion *segmentDeleteRunner
	// summarizer returns the summarizer of the settings saved last, nil
	// while summaries are off (summarizers.summarizer).
	summarizer                    func() unitSummarizer
	hub                           summaryBroadcaster
	publishSummaryReady           func(sessionID string)
	recordMergedRefinementFailure func(context.Context, string, error)
	restoreFromDrive              func(ctx context.Context) (*gdrive.RestoreResult, error)
	// cfgStore holds summarization.batch_jobs, read at each batch backfill.
	cfgStore           interface{ Get() config.Config }
	submitSummaryBatch func(ctx context.Context, sessionIDs []string) (batchName string, count int, err error)
	pollSummaryBatch   func(ctx context.Context, batchName string) (map[string]any, error)
}

// wireStoreWork gives hooks the requests' work on the store. Each request
// runs as work the stop cuts off and waits for (AdmitRequest, through
// session.Manager.Run), since a handler may write the store itself (a
// title, a deletion), and the HTTP server's shutdown does not end a handler
// whose connection it closed. Each piece that outlasts its request (a
// session's end), or that can outlast a moment (a summary repair, a restore
// from Drive, a batch submit or poll, a backlog's online summaries), runs the
// same way (session.Manager.Go and Run), so the store closes under none of
// it.
func wireStoreWork(hooks *server.ControlHooks, w *storeWork) {
	hooks.AdmitRequest = admitRequestsThrough(w.manager)
	hooks.GoStoreWork = w.manager.Go
	// RetrySync starts a sync of the session in the background and says at
	// once if none can start (for a session whose Drive folder was deleted,
	// while Drive says it is not back), so the API does not report a retry
	// that does nothing.
	hooks.RetrySync = func(ctx context.Context, sessionID string) error {
		return retrySync(ctx, w.manager, w.drive.Get(), sessionID)
	}
	hooks.RefinementRecordingAvailable = w.deletion.checkRefinementRecording
	hooks.RetryRefinement = w.deletion.retryRefinement
	hooks.RepairSummaries = func(ctx context.Context) (repaired int, err error) {
		err = w.manager.Run(ctx, func(ctx context.Context) error {
			repaired, err = repairStructuredSummaries(ctx, w.store, w.publishSummaryReady)
			return err
		})
		return repaired, err
	}
	hooks.Resummarize = resummarizeUntilStop(w.manager, w.store, w.summarizer, w.hub, w.publishSummaryReady)
	hooks.OnSessionMerged = mergedSessionHook(w.manager, w.store, w.summarizer, w.hub, w.publishSummaryReady, w.deletion.retryRefinement, w.recordMergedRefinementFailure)
	hooks.EndSession = w.manager.ForceEndSession
	hooks.StartSession = w.manager.ManualStartSession
	hooks.StopSession = w.manager.StopSession
	hooks.RestoreFromGDrive = func(ctx context.Context) (restored map[string]any, err error) {
		err = w.manager.Run(ctx, func(ctx context.Context) error {
			result, err := w.restoreFromDrive(ctx)
			if err != nil {
				return err
			}
			restored = map[string]any{
				"restored": result.Restored,
				"skipped":  result.Skipped,
				"errors":   result.Errors,
			}
			return nil
		})
		return restored, err
	}
	// A backlog's online summaries outlive the request that asked for them
	// (POST /api/sessions/backfill, and POST /api/sessions/batch-backfill
	// with batch jobs off or not taken).
	later := backlogUntilStop(w.manager, newOnlineSummaries(w.store, w.summarizer, w.hub, w.publishSummaryReady))
	hooks.SummarizeBacklog = later
	batchBackfill := newBatchBackfill(w.store, w.summarizer, w.cfgStore, w.submitSummaryBatch, later)
	hooks.BatchBackfill = func(ctx context.Context) (batchName string, count int, err error) {
		err = w.manager.Run(ctx, func(ctx context.Context) error {
			batchName, count, err = batchBackfill(ctx)
			return err
		})
		return batchName, count, err
	}
	hooks.BatchPoll = func(ctx context.Context, batchName string) (result map[string]any, err error) {
		err = w.manager.Run(ctx, func(ctx context.Context) error {
			result, err = w.pollSummaryBatch(ctx, batchName)
			return err
		})
		return result, err
	}
}

// admitRequestsThrough is the requests' admission (ControlHooks.AdmitRequest):
// each request runs as work manager's stop cuts off and waits for, and none
// runs once the stop has run.
func admitRequestsThrough(manager *session.Manager) func(ctx context.Context, serve func(ctx context.Context)) error {
	return func(ctx context.Context, serve func(ctx context.Context)) error {
		return manager.Run(ctx, func(ctx context.Context) error {
			serve(ctx)
			return nil
		})
	}
}
