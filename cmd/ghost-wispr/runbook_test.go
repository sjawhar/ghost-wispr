package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// runbookSection returns the section of docs/pi-setup.md under heading, its
// words joined by single spaces, so a quote the Markdown wraps still matches.
func runbookSection(t *testing.T, heading string) string {
	t.Helper()
	runbook, err := os.ReadFile(filepath.Join("..", "..", "docs", "pi-setup.md"))
	if err != nil {
		t.Fatalf("read the Pi runbook: %v", err)
	}
	_, section, found := strings.Cut(string(runbook), "\n"+heading+"\n")
	if !found {
		t.Fatalf("the Pi runbook has no %q section", heading)
	}
	section, _, _ = strings.Cut(section, "\n## ")
	return strings.Join(strings.Fields(section), " ")
}

// TestRunbookSaysWhatToDoWhenDeleteWaitsForDrive fails if the runbook's remedy
// for a session kept on this unit only, DELETE /api/sessions/{id}, does not
// say what to do when that delete answers 409 because a segment deletion
// still owes the session's Drive folder, which happens while that folder is
// in the Drive trash.
func TestRunbookSaysWhatToDoWhenDeleteWaitsForDrive(t *testing.T) {
	section := runbookSection(t, "## Sessions kept on this unit only")
	refusal := storage.ErrDeleteReplacementPending.Error()
	if !strings.Contains(section, refusal) {
		t.Fatalf("the runbook's \"Sessions kept on this unit only\" section does not quote the 409 %q", refusal)
	}
	for _, step := range []string{"Delete forever", "jq .session.drive_replace_pending"} {
		if !strings.Contains(section, step) {
			t.Errorf("the runbook's \"Sessions kept on this unit only\" section does not say %q", step)
		}
	}
}
