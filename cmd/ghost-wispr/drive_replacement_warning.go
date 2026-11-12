package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const driveReplacementWarningKeyPrefix = "drive-replacement:"

// A session that owes Google Drive a replacement after a segment deletion is
// reported on GET /api/status once it has owed it for driveReplacementWarnAfter
// (a replacement that only waits: for a splice, a summary, a folder in the
// trash), or once driveReplacementWarnFailures attempts in a row failed.
const (
	driveReplacementWarnAfter    = time.Hour
	driveReplacementWarnFailures = 3
)

type owedReplacementStore interface {
	OwedDriveReplacements() ([]storage.OwedDriveReplacement, error)
}

// refreshDriveReplacementWarnings sets one warning on registry for each
// session the store says owes Drive a replacement past either bound, and
// clears the warning of every other session. It reads only the store, so a
// warning is back after a restart and covers a replacement that never fails
// but never finishes either.
func refreshDriveReplacementWarnings(store owedReplacementStore, registry *warningsRegistry, now time.Time) error {
	owed, err := store.OwedDriveReplacements()
	if err != nil {
		return fmt.Errorf("read owed Drive replacements: %w", err)
	}
	warnings := make(map[string]string, len(owed))
	for _, replacement := range owed {
		age := now.Sub(replacement.Since)
		if replacement.Failures < driveReplacementWarnFailures && !replacement.Since.IsZero() && age < driveReplacementWarnAfter {
			continue
		}
		var reason strings.Builder
		if replacement.Since.IsZero() {
			reason.WriteString("owed since before this version")
		} else {
			fmt.Fprintf(&reason, "owed for %s", age.Round(time.Minute))
		}
		if replacement.Failures > 0 {
			fmt.Fprintf(&reason, "; %d attempts in a row failed, the last with: %s", replacement.Failures, replacement.LastError)
		}
		warnings[driveReplacementWarningKeyPrefix+replacement.SessionID] = fmt.Sprintf(
			"Google Drive still holds the words of a deleted segment of session %s (%s). The sync sweep keeps retrying.",
			replacement.SessionID, reason.String())
	}
	registry.ReplacePrefix(driveReplacementWarningKeyPrefix, warnings)
	return nil
}
