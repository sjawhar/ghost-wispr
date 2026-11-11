package main

import "fmt"

const pendingSpliceWarningKeyPrefix = "pending-segment-splices:"

func setPendingSpliceWarning(registry *warningsRegistry, sessionID, recording string, recoveryErr error) {
	subject := sessionID
	if recording != "" {
		subject += ":" + recording
	}
	if subject == "" {
		subject = "startup"
	}
	registry.Set(pendingSpliceWarningKeyPrefix+subject, fmt.Sprintf("A segment deletion needs audio recovery: %v", recoveryErr))
}
