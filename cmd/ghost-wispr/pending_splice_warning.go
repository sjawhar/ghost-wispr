package main

import (
	"fmt"
	"log"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const (
	pendingSpliceWarningKeyPrefix   = "pending-segment-splices:"
	pendingSpliceCleanupWarningKey  = pendingSpliceWarningKeyPrefix + "startup-cleanup"
	pendingSpliceRecoveryWarningKey = pendingSpliceWarningKeyPrefix + "startup-recovery"
)

// pendingSpliceWarningKey identifies one recording of one session, so a
// successful recovery clears only that recording's warning.
func pendingSpliceWarningKey(sessionID, recording string) string {
	return pendingSpliceWarningKeyPrefix + sessionID + "\x00" + recording
}

func pendingSpliceWarningText(recoveryErr error) string {
	return fmt.Sprintf("A segment deletion needs audio recovery: %v", recoveryErr)
}

func setPendingSpliceWarning(registry *warningsRegistry, sessionID, recording string, recoveryErr error) {
	registry.Set(pendingSpliceWarningKey(sessionID, recording), pendingSpliceWarningText(recoveryErr))
}

func clearPendingSpliceWarning(registry *warningsRegistry, sessionID, recording string) {
	registry.Clear(pendingSpliceWarningKey(sessionID, recording))
}

func pendingSpliceWarningCallbacks(registry *warningsRegistry) (func(storage.DeletedSpan, error), func(string, string)) {
	return func(span storage.DeletedSpan, recoveryErr error) {
			log.Printf("warning: pending segment splice session=%s recording=%s: %v", span.SessionID, span.AudioPath, recoveryErr)
			setPendingSpliceWarning(registry, span.SessionID, span.AudioPath, recoveryErr)
		}, func(sessionID, recording string) {
			clearPendingSpliceWarning(registry, sessionID, recording)
		}
}

func recordPendingSpliceCleanupWarning(registry *warningsRegistry, recoveryErr error) {
	registry.Set(pendingSpliceCleanupWarningKey, pendingSpliceWarningText(recoveryErr))
}

func recordPendingSpliceRecoveryWarning(registry *warningsRegistry, recoveryErr error) {
	registry.Set(pendingSpliceRecoveryWarningKey, pendingSpliceWarningText(recoveryErr))
}
