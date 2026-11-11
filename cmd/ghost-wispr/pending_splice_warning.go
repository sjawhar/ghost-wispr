package main

import (
	"errors"
	"fmt"
	"log"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const (
	pendingSpliceWarningKeyPrefix    = "pending-segment-splices:"
	pendingSpliceRefinementKeyPrefix = "segment-deletion-refinement:"
	pendingSpliceCleanupWarningKey   = pendingSpliceWarningKeyPrefix + "startup-cleanup"
	pendingSpliceRecoveryWarningKey  = pendingSpliceWarningKeyPrefix + "startup-recovery"
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

func pendingSpliceRefinementKey(span *storage.DeletedSpan) string {
	return pendingSpliceRefinementKeyPrefix + span.SessionID + "\x00" + span.AudioPath
}

func applyPendingSpliceRecoveryWarnings(registry *warningsRegistry, recovered []storage.DeletedSpan, failed []pendingSpliceFailure) {
	for i := range recovered {
		clearPendingSpliceWarning(registry, recovered[i].SessionID, recovered[i].AudioPath)
	}
	for i := range failed {
		failure := failed[i]
		var refinementErr refinementAfterSegmentDeletionError
		if errors.As(failure.Err, &refinementErr) {
			log.Printf("warning: %v recording=%s", failure.Err, failure.Span.AudioPath)
			registry.Set(pendingSpliceRefinementKey(&failure.Span), failure.Err.Error())
			continue
		}
		log.Printf("warning: pending segment splice session=%s recording=%s: %v", failure.Span.SessionID, failure.Span.AudioPath, failure.Err)
		setPendingSpliceWarning(registry, failure.Span.SessionID, failure.Span.AudioPath, failure.Err)
	}
}

func recordPendingSpliceCleanupWarning(registry *warningsRegistry, recoveryErr error) {
	registry.Set(pendingSpliceCleanupWarningKey, pendingSpliceWarningText(recoveryErr))
}

func recordPendingSpliceRecoveryWarning(registry *warningsRegistry, recoveryErr error) {
	registry.Set(pendingSpliceRecoveryWarningKey, pendingSpliceWarningText(recoveryErr))
}
