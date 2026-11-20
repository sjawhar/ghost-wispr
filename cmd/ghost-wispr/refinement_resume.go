package main

import "context"

// resumeStoppedRefinements runs again each refinement a stop of the app cut
// off (storage.RecoverStaleRefinements), one at a time and in order, with the
// "Retry refinement" action (retry), so a restart during a meeting's
// refinement does not leave it failed until a person presses Retry. A run that
// fails leaves its session failed, with the action's status warning, and the
// next one still runs. It stops when ctx ends.
func resumeStoppedRefinements(ctx context.Context, sessionIDs []string, retry func(context.Context, string) error, logf func(string, ...any)) {
	for _, id := range sessionIDs {
		if ctx.Err() != nil {
			return
		}
		logf("resuming the refinement of session %s, which the last stop cut off", id)
		_ = retry(ctx, id)
	}
}
