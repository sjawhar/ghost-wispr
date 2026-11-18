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
	for _, safeguard := range []string{"gdrive_folder_id", "Save no settings", "installed commit", "active session", "models step again", "outside meetings", "only the full provisioner installs"} {
		if !strings.Contains(section, safeguard) {
			t.Errorf("the runbook's rollout order does not say %q", safeguard)
		}
	}
}

// TestTheJabraButtonIsOnInTheTemplateAndTheRunbook fails if the office Pi's
// template does not turn the Jabra's Mute button on, or if the template, the
// rollout order or "Pause and resume at the unit" still keeps the button off
// until a person's taps pass or until a follow-up fix merges: Sami turned it
// on on the office Pi (2026-11-18), and the full provisioner rewrites .env,
// so the template's setting is the Pi's. It also fails if the rollout order
// does not say what its steps do to the Pi's .env override, or checks for the
// button off; or if "Pause and resume at the unit" drops the rule that no
// other program opens the Jabra while the button is on.
func TestTheJabraButtonIsOnInTheTemplateAndTheRunbook(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "pi", "files", "ghost-wispr.yaml"))
	if err != nil {
		t.Fatalf("read the Pi's config template: %v", err)
	}
	template := string(raw)
	if !strings.Contains(template, "\njabra_mute_button: true\n") || strings.Contains(template, "jabra_mute_button: false") {
		t.Error("the Pi's config template does not turn the Jabra's Mute button on")
	}
	rollout := runbookSection(t, "## Roll out a release that adds a config key")
	pause := runbookSection(t, "## Pause and resume at the unit")
	for name, text := range map[string]string{
		"the rollout order":              rollout,
		`"Pause and resume at the unit"`: pause,
		"the Pi's config template":       strings.ReplaceAll(strings.Join(strings.Fields(template), " "), "# ", ""),
	} {
		for _, stale := range []string{"real taps", "before the recording light reads red", "stays off either way", "until a person", "follow-up fix", "keeps `jabra_mute_button: false`"} {
			if strings.Contains(text, stale) {
				t.Errorf("%s still keeps the Jabra's button off: %q", name, stale)
			}
		}
	}
	for _, words := range []string{"GHOST_WISPR_JABRA_MUTE_BUTTON", "keeps every `.env` line", "rewrites `.env`", "jabra_mute_button: true", "ends in `true`"} {
		if !strings.Contains(rollout, words) {
			t.Errorf("the runbook's rollout order does not say %q", words)
		}
	}
	if strings.Contains(rollout, "ends in `false`") {
		t.Error("the runbook's rollout order checks that the Jabra's Mute button is off")
	}
	if !strings.Contains(pause, "Do not open the Jabra's microphone or speaker with another program") {
		t.Error(`"Pause and resume at the unit" no longer says that no other program may open the Jabra while the button is on`)
	}
}

// TestRunbookInjectsSpeechThroughMicDevice fails if the runbook does not say
// how to inject speech for an acceptance run with mic_device: load the ALSA
// loopback, set GHOST_WISPR_MIC_DEVICE=Loopback in the Pi's .env for the run,
// play into the loopback device the recorder captures from, then remove the
// line and unload the loopback. With mic_device: J75 the recorder opens no
// other card, so the old way, an .asoundrc that moved ALSA's default input to
// the loopback, no longer reaches it.
func TestRunbookInjectsSpeechThroughMicDevice(t *testing.T) {
	section := runbookSection(t, "## Inject speech for acceptance")
	for _, step := range []string{"modprobe snd-aloop", "GHOST_WISPR_MIC_DEVICE=Loopback", "/opt/ghost-wispr/.env", "plughw:CARD=Loopback,DEV=1", "remove", "modprobe -r snd-aloop"} {
		if !strings.Contains(section, step) {
			t.Errorf("the runbook's \"Inject speech for acceptance\" does not say %q", step)
		}
	}
	if strings.Contains(section, "asoundrc") {
		t.Error("the runbook's \"Inject speech for acceptance\" still routes the input with an .asoundrc")
	}
}

// TestRunbookRolloutChecksTheMicDevice fails if the rollout order does not
// name mic_device among the keys a release adds, and its check: the Pi's
// config holds mic_device: J75.
func TestRunbookRolloutChecksTheMicDevice(t *testing.T) {
	section := runbookSection(t, "## Roll out a release that adds a config key")
	for _, words := range []string{"`mic_device`", "mic_device: J75"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's rollout order does not say %q", words)
		}
	}
}
