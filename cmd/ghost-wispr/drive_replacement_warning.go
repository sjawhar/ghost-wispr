package main

import (
	"log/slog"

	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const driveReplacementWarningKeyPrefix = "drive-replacement:"

// driveReplacementWarnings returns the hooks that put a session's failing
// Drive replacement on GET /api/status, one warning per session, and take it
// off once the session no longer owes Drive a replacement.
func driveReplacementWarnings(registry *warningsRegistry) (set func(sessionID, message string), clear func(sessionID string)) {
	return func(sessionID, message string) {
			registry.Set(driveReplacementWarningKeyPrefix+sessionID, message)
		}, func(sessionID string) {
			registry.Clear(driveReplacementWarningKeyPrefix + sessionID)
		}
}

// newDriveOrchestrator is the app's Drive sync orchestrator: syncer over
// store, reporting failing replacements on warnings.
func newDriveOrchestrator(syncer *gdrive.Syncer, store *storage.SQLiteStore, logger *slog.Logger, warnings *warningsRegistry) *gdrive.Orchestrator {
	orchestrator := gdrive.NewOrchestrator(syncer, store, logger)
	orchestrator.SetReplacementWarnings(driveReplacementWarnings(warnings))
	return orchestrator
}
