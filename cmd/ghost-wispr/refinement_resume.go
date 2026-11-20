package main

import (
	"context"
	"errors"
	"log/slog"
)

// refinementResumeStore is what the start's automatic refinement retry keeps
// in the store (storage.RecoverStaleRefinements).
type refinementResumeStore interface {
	RefinementResumes() (pending, exhausted []string, err error)
	ClaimRefinementResume(id string) (bool, error)
	FinishRefinementResume(id string) error
}

// errRefinementRetryCutOff is why a refinement whose automatic retry was cut
// off is not retried again on its own.
var errRefinementRetryCutOff = errors.New("a stop cut off its refinement, and another stop cut off the automatic retry at the next start; it is not retried again on its own: press Retry refinement")

// resumeStoppedRefinements runs, at the start, the automatic retry the store
// owes each refinement a stop cut off (storage.RecoverStaleRefinements), one
// at a time and oldest first, with the "Retry refinement" action (retry), so a
// restart during a meeting's refinement does not leave it failed until a
// person presses Retry. Each retry is recorded before it starts, so a crash or
// a hang in it is not followed by another: at the next start that refinement
// keeps failed, with Retry's warning, until a person's Retry. A retry that
// fails leaves its session failed, with that warning, and the next one still
// runs. It stops when ctx ends; the retries it has not reached stay owed, and
// the one it is in, cut off, is not retried again on its own.
func resumeStoppedRefinements(ctx context.Context, store refinementResumeStore, warnings *warningsRegistry, retry func(context.Context, string) error) {
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
		_ = retry(ctx, id)
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
