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

// TestRunbookOrdersARolloutThatAddsAConfigKey fails if the runbook does not
// give the order for a release that reads a new key from the Pi's config:
// install the template with the models step just before the merge, merge,
// run update.sh by hand once the release is published, then check GET
// /api/config. It also fails if the runbook does not say why: the nightly
// update replaces only the binary, and a settings save of the old binary
// drops a key that binary does not know. And it fails if a step can fail
// without anyone noticing: the runbook must say where the shared-drive ID
// comes from, to save no settings until the check, that update.sh is done
// only when it says it installed the commit and does nothing during an active
// session, what to do when the check fails, and that restarts end a meeting.
func TestRunbookOrdersARolloutThatAddsAConfigKey(t *testing.T) {
	section := runbookSection(t, "## Roll out a release that adds a config key")
	rest := section
	for _, step := range []string{"1. Just before you merge", "provision.sh --models", "2. Merge", "3. As soon as", "update.sh", "4. Check", "/api/config"} {
		_, after, found := strings.Cut(rest, step)
		if !found {
			t.Fatalf("the runbook's rollout order is missing %q, or has it out of order: %s", step, section)
		}
		rest = after
	}
	for _, why := range []string{"replaces only the binary", "drops"} {
		if !strings.Contains(section, why) {
			t.Errorf("the runbook's rollout order does not say why: no %q", why)
		}
	}
	for _, safeguard := range []string{"gdrive_folder_id", "Save no settings", "installed commit", "active session", "models step again", "outside meetings"} {
		if !strings.Contains(section, safeguard) {
			t.Errorf("the runbook's rollout order does not say %q", safeguard)
		}
	}
}

// TestRunbookRolloutKeepsTheJabraOverride fails if the rollout order does not
// say what its steps do to a setting the office Pi holds in .env rather than
// in its YAML, the Jabra Mute button (GHOST_WISPR_JABRA_MUTE_BUTTON): the
// models step keeps that line as it is, the full provisioner rewrites .env and
// the template's jabra_mute_button: false applies, and the button stays off
// until the fix of defect D9 ships ("Pause and resume at the unit"). It also
// fails if the rollout writes or checks for the button turned on: a restart of
// a paused unit with the button on can resume recording by itself.
func TestRunbookRolloutKeepsTheJabraOverride(t *testing.T) {
	section := runbookSection(t, "## Roll out a release that adds a config key")
	for _, words := range []string{"GHOST_WISPR_JABRA_MUTE_BUTTON", "jabra_mute_button: false", "keeps every `.env` line", "rewrites `.env`", "D9", `"Pause and resume at the unit"`} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's rollout order does not say %q", words)
		}
	}
	for _, on := range []string{"GHOST_WISPR_JABRA_MUTE_BUTTON=true", "jabra_mute_button: true"} {
		if strings.Contains(section, on) {
			t.Errorf("the runbook's rollout order turns the Jabra's Mute button on: %q", on)
		}
	}
}
