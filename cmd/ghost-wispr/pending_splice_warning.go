package main

import "fmt"

const pendingSpliceWarningKeyPrefix = "pending-segment-splices:"

func setPendingSpliceWarning(registry *warningsRegistry, recoveryErr error) {
	registry.Set(pendingSpliceWarningKeyPrefix+recoveryErr.Error(), fmt.Sprintf("A segment deletion needs audio recovery: %v", recoveryErr))
}
