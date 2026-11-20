package main

import (
	"context"
	"log/slog"
	"time"
)

// lifecyclePublishWait bounds the stop's wait for the current session's
// lifecycle events to reach the hub (session.Manager.WaitForLifecyclePublishes).
const lifecyclePublishWait = 5 * time.Second

// shutdownSessionEnder is the part of the session manager the stop drives.
type shutdownSessionEnder interface {
	ForceEndSession(ctx context.Context) error
	WaitForLifecyclePublishes(ctx context.Context) error
}

// endSessionAtShutdown ends the current session and waits, for lifecycleWait
// at most and never past shutdownCtx's deadline, for its lifecycle events and
// for any session end under way (a pause's), so the stop closes the database
// under none of them and still ends on time.
func endSessionAtShutdown(shutdownCtx context.Context, manager shutdownSessionEnder, lifecycleWait time.Duration) {
	if err := manager.ForceEndSession(shutdownCtx); err != nil {
		slog.Warn("force end session failed", "error", err)
	}
	lifecycleCtx, cancel := context.WithTimeout(shutdownCtx, lifecycleWait)
	defer cancel()
	if err := manager.WaitForLifecyclePublishes(lifecycleCtx); err != nil {
		slog.Warn("lifecycle event publishing did not finish before shutdown", "error", err)
	}
}
