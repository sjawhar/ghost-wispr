package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const (
	pendingSpliceWarningKeyPrefix    = "pending-segment-splices:"
	pendingSpliceRefinementKeyPrefix = "segment-deletion-refinement:"
	pendingSpliceCleanupWarningKey   = pendingSpliceWarningKeyPrefix + "startup-cleanup"
	pendingSpliceRecoveryWarningKey  = pendingSpliceWarningKeyPrefix + "startup-recovery"
)

func segmentDeletionWarningKey(prefix, sessionID, recording string) string {
	return prefix + sessionID + "\x00" + recording
}

func setPendingSpliceWarning(registry *warningsRegistry, sessionID, recording string, recoveryErr error) {
	registry.Set(segmentDeletionWarningKey(pendingSpliceWarningKeyPrefix, sessionID, recording),
		fmt.Sprintf("A segment deletion for session %s needs audio recovery: %v", sessionID, recoveryErr))
}

func clearPendingSpliceWarning(registry *warningsRegistry, sessionID, recording string) {
	registry.Clear(segmentDeletionWarningKey(pendingSpliceWarningKeyPrefix, sessionID, recording))
}

// refinementWarningText names the innermost cause of err: it follows %w
// wrapping to the last error, then keeps the final ": "-separated clause of
// that error's text, which is the innermost cause of a chain flattened by %v.
func refinementWarningText(sessionID string, err error) string {
	for inner := errors.Unwrap(err); inner != nil; inner = errors.Unwrap(err) {
		err = inner
	}
	text := strings.TrimSpace(err.Error())
	if i := strings.LastIndex(text, ": "); i >= 0 {
		text = strings.TrimSpace(text[i+2:])
	}
	if text == "" {
		text = "unknown error"
	}
	return fmt.Sprintf("Refinement after segment deletion failed for session %s: %s", sessionID, text)
}

func applyPendingSpliceRecoveryWarnings(registry *warningsRegistry, outcomes []pendingSpliceOutcome) {
	for i := range outcomes {
		outcome := outcomes[i]
		if outcome.Recovered {
			clearPendingSpliceWarning(registry, outcome.Span.SessionID, outcome.Span.AudioPath)
		}
		if outcome.Err == nil {
			continue
		}
		var refinementErr refinementAfterSegmentDeletionError
		if errors.As(outcome.Err, &refinementErr) {
			slog.Warn("refinement after segment deletion failed", "session_id", refinementErr.sessionID, "recording", outcome.Span.AudioPath, "error", refinementErr.err)
			registry.Set(segmentDeletionWarningKey(pendingSpliceRefinementKeyPrefix, refinementErr.sessionID, ""), refinementWarningText(refinementErr.sessionID, refinementErr.err))
			continue
		}
		slog.Warn("pending segment splice failed", "session_id", outcome.Span.SessionID, "recording", outcome.Span.AudioPath, "error", outcome.Err)
		setPendingSpliceWarning(registry, outcome.Span.SessionID, outcome.Span.AudioPath, outcome.Err)
	}
}

type refinementRetryStore interface {
	GetSession(id string) (storage.Session, error)
}

func retryRefinementWithWarningClear(ctx context.Context, registry *warningsRegistry, store refinementRetryStore, sessionID string, refine func(context.Context, string, string, time.Time) error) error {
	sess, err := store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s before retry refinement: %w", sessionID, err)
	}
	if err := refine(ctx, sessionID, sess.AudioPath, sess.StartedAt); err != nil {
		return err
	}
	sess, err = store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s after retry refinement: %w", sessionID, err)
	}
	if sess.RefinementStatus == storage.RefinementCompleted {
		registry.Clear(segmentDeletionWarningKey(pendingSpliceRefinementKeyPrefix, sessionID, ""))
	}
	return nil
}

// recordPendingSpliceCleanupWarning and recordPendingSpliceRecoveryWarning
// put on GET /api/status that a start-up step of segment deletions failed,
// and log why (err). They are warnings of the whole unit, which no delete
// clears, and err can name a recording, which is named for its session: the
// warning says which step failed, and only the log line holds err.
func recordPendingSpliceCleanupWarning(registry *warningsRegistry, err error) {
	slog.Warn("failed to clean leftover splice copies", "error", err)
	registry.Set(pendingSpliceCleanupWarningKey, "Leftover copies of a segment deletion's recording could not be cleaned up at startup; the app's log says why.")
}

func recordPendingSpliceRecoveryWarning(registry *warningsRegistry, err error) {
	slog.Warn("failed to recover pending segment splices", "error", err)
	registry.Set(pendingSpliceRecoveryWarningKey, "A segment deletion's audio recovery failed at startup; the app's log says why.")
}
