package main

import (
	"errors"
	"strings"
	"testing"
)

func TestR4PendingSpliceWarningsKeepEachRecording(t *testing.T) {
	registry := newWarningsRegistry()
	setPendingSpliceWarning(registry, errors.New("re-splice /recordings/first.mp3 failed"))
	setPendingSpliceWarning(registry, errors.New("re-splice /recordings/second.mp3 failed"))
	warnings := registry.List()
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), "/recordings/first.mp3") || !strings.Contains(strings.Join(warnings, "\n"), "/recordings/second.mp3") {
		t.Fatalf("pending warnings = %v, want both recording paths", warnings)
	}
}
