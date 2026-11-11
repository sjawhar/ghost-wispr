package main

import (
	"fmt"
	"strings"
)

const pendingSpliceWarningKeyPrefix = "pending-segment-splices:"

func setPendingSpliceWarning(registry *warningsRegistry, recoveryErr error) {
	registry.Set(pendingSpliceWarningKeyPrefix+pendingSpliceWarningSubject(recoveryErr), fmt.Sprintf("A segment deletion needs audio recovery: %v", recoveryErr))
}

func pendingSpliceWarningSubject(recoveryErr error) string {
	for _, field := range strings.Fields(recoveryErr.Error()) {
		field = strings.Trim(field, ":,")
		if strings.ContainsRune(field, '/') {
			return field
		}
	}
	return "startup"
}
