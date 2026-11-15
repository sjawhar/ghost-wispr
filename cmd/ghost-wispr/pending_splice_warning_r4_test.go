package main

import (
	"errors"
	"strings"
	"testing"
)

func TestR4PendingSpliceWarningsKeepEachRecording(t *testing.T) {
	registry := newWarningsRegistry()
	setPendingSpliceWarning(registry, "s1", "/recordings/first.mp3", errors.New("re-splice /recordings/first.mp3 failed"))
	setPendingSpliceWarning(registry, "s2", "/recordings/second.mp3", errors.New("re-splice /recordings/second.mp3 failed"))
	warnings := registry.List()
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), "/recordings/first.mp3") || !strings.Contains(strings.Join(warnings, "\n"), "/recordings/second.mp3") {
		t.Fatalf("pending warnings = %v, want both recording paths", warnings)
	}
}

func TestPendingSpliceWarningKeyIgnoresErrorText(t *testing.T) {
	registry := newWarningsRegistry()
	setPendingSpliceWarning(registry, "s1", "/recordings/first.mp3", errors.New("re-splice /recordings/first.mp3 timed out"))
	setPendingSpliceWarning(registry, "s1", "/recordings/first.mp3", errors.New("replace recovered recording /recordings/first.mp3 failed"))
	if warnings := registry.List(); len(warnings) != 1 || !strings.Contains(warnings[0], "failed") {
		t.Fatalf("warnings = %v, want one current warning for the recording", warnings)
	}
}

func TestPendingSpliceWarningsWithoutPathsKeepSessionsSeparate(t *testing.T) {
	registry := newWarningsRegistry()
	setPendingSpliceWarning(registry, "s1", "", errors.New("refinement failed"))
	setPendingSpliceWarning(registry, "s2", "", errors.New("refinement failed"))
	if warnings := registry.List(); len(warnings) != 2 {
		t.Fatalf("warnings = %v, want distinct warnings for both sessions", warnings)
	}
}
