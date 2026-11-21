package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// lifecyclePublishWait bounds the stop's wait for the current session's
// lifecycle events to reach the hub (session.Manager.WaitForLifecyclePublishes).
const lifecyclePublishWait = 5 * time.Second

// stopRecoveryStore is what the app keeps in the store about the
// refinements a stop cuts off (storage.RecoverStaleRefinements).
type stopRecoveryStore interface {
	RecoverStaleRefinements() error
	RefinementResumes() (pending, exhausted []string, err error)
	ClaimRefinementResume(id string) (bool, error)
	FinishRefinementResume(id string) error
}

// shutdownSessionEnder is the part of the session manager the stop drives.
type shutdownSessionEnder interface {
	ForceEndSession(ctx context.Context) error
	WaitForLifecyclePublishes(ctx context.Context) error
	StopBackground(ctx context.Context) error
}

// stopRecovery is what the app does about the work a stop cuts off: a
// session's refinement, its session end and that end's events. main makes it
// at the start (recoverAtStart), starts its retries once the recording
// directories are cleaned up (retry), and ends with it at the stop (stop).
type stopRecovery struct {
	store   stopRecoveryStore
	retries sync.WaitGroup
}

// recoverAtStart marks failed every refinement the last stop cut off and
// records, in the store, the one automatic retry the start owes each
// (storage.RecoverStaleRefinements). It must run before anything can start a
// refinement: one this run started would be taken for one a stop cut off.
func recoverAtStart(store stopRecoveryStore) *stopRecovery {
	if err := store.RecoverStaleRefinements(); err != nil {
		slog.Warn("failed to recover stale refinements", "error", err)
	}
	return &stopRecovery{store: store}
}

// retry runs in the background, one at a time and oldest first, the
// automatic retry the store owes each refinement a stop cut off, with the
// "Retry refinement" action (retryRefinement), so a restart during a
// meeting's refinement does not leave it failed until a person presses Retry.
// It runs after the recording directories are cleaned up, since a retry may
// restore recordings. Each retry is recorded before it starts, so a crash or
// a hang in it is not followed by another: from the next start on, that
// refinement keeps failed, with Retry's warning, until a person's Retry. A
// retry that fails leaves its session failed, with that warning, and the next
// one still runs. The retries stop when ctx ends; those not reached stay owed,
// and the one cut off is not retried again on its own.
func (r *stopRecovery) retry(ctx context.Context, warnings *warningsRegistry, retryRefinement func(context.Context, string) error) {
	r.retries.Add(1)
	go func() {
		defer r.retries.Done()
		resumeStoppedRefinements(ctx, r.store, warnings, retryRefinement)
	}()
}

// stop ends the current session and waits, within shutdownCtx's deadline, for
// any session end under way (a pause's), for the session's lifecycle events
// (for lifecycleWait at most), for the work the session ends left running
// (their refinements and summaries), which the stop cuts off, and for the
// start's retries, which the stop has cancelled, so the database closes under
// none of them and the stop still ends on time. A refinement or summary cut
// off stays running in the store, for the next start (session.ErrStopped).
func (r *stopRecovery) stop(shutdownCtx context.Context, manager shutdownSessionEnder, lifecycleWait time.Duration) {
	if err := manager.ForceEndSession(shutdownCtx); err != nil {
		slog.Warn("force end session failed", "error", err)
	}
	lifecycleCtx, cancel := context.WithTimeout(shutdownCtx, lifecycleWait)
	defer cancel()
	if err := manager.WaitForLifecyclePublishes(lifecycleCtx); err != nil {
		slog.Warn("lifecycle event publishing did not finish before shutdown", "error", err)
	}
	if err := manager.StopBackground(shutdownCtx); err != nil {
		slog.Warn("the work the session ends left running did not end before shutdown", "error", err)
	}
	retried := make(chan struct{})
	go func() {
		r.retries.Wait()
		close(retried)
	}()
	select {
	case <-retried:
	case <-shutdownCtx.Done():
		slog.Warn("the refinement retries a stop cut off did not end before shutdown", "error", shutdownCtx.Err())
	}
}

// errRefinementRetryCutOff is why a refinement whose automatic retry was cut
// off is not retried again on its own.
var errRefinementRetryCutOff = errors.New("a stop cut off its refinement, and another stop cut off the automatic retry at the next start; it is not retried again on its own: press Retry refinement")

// resumeStoppedRefinements is the retries' loop (stopRecovery.retry).
func resumeStoppedRefinements(ctx context.Context, store stopRecoveryStore, warnings *warningsRegistry, retryRefinement func(context.Context, string) error) {
	pending, exhausted, err := store.RefinementResumes()
	if err != nil {
		slog.Warn("the refinements a stop cut off are not retried", "error", err)
		return
	}
	for _, id := range exhausted {
		recordRefinementRetryFailure(warnings, id, errRefinementRetryCutOff)
	}
	for _, id := range pending {
		if ctx.Err() != nil {
			return
		}
		claimed, err := store.ClaimRefinementResume(id)
		if err != nil {
			slog.Warn("the refinement a stop cut off is not retried", "session_id", id, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		slog.Info("retrying the refinement a stop cut off", "session_id", id)
		_ = retryRefinement(ctx, id)
		if ctx.Err() != nil {
			// A stop cut this retry off: it stays claimed, so the next start
			// leaves the refinement failed, with Retry's warning.
			return
		}
		if err := store.FinishRefinementResume(id); err != nil {
			slog.Warn("the end of an automatic refinement retry is not recorded", "session_id", id, "error", err)
		}
	}
}
