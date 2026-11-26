package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// readRunbook returns docs/pi-setup.md.
func readRunbook(t *testing.T) string {
	t.Helper()
	runbook, err := os.ReadFile(filepath.Join("..", "..", "docs", "pi-setup.md"))
	if err != nil {
		t.Fatalf("read the Pi runbook: %v", err)
	}
	return string(runbook)
}

// runbookSection returns the section of docs/pi-setup.md under heading, its
// words joined by single spaces, so a quote the Markdown wraps still matches.
func runbookSection(t *testing.T, heading string) string {
	t.Helper()
	_, section, found := strings.Cut(readRunbook(t), "\n"+heading+"\n")
	if !found {
		t.Fatalf("the Pi runbook has no %q section", heading)
	}
	section, _, _ = strings.Cut(section, "\n## ")
	return strings.Join(strings.Fields(section), " ")
}

// releaseSection is the runbook's one section on a release that changes the
// Pi's config.
const releaseSection = "## A release that changes config"

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

// TestRunbookSaysAReleaseInstallsItsConfigWithItsBinary fails if the
// runbook's section on a release that changes config does not say that the
// nightly update installs the release's whole deploy set together, the config
// with the binary, so a new binary never starts on an old config; that
// settings saved on the page stay in ghost-wispr.local.yaml; that the update
// leaves a busy recorder alone and restores the previous release when the new
// one does not answer; that a changed provisioning file needs the release's
// provision.sh; that deploy/pi/check-deploy.sh is the check; and that a release
// that answers its commit but fails a check is reverted on main, whose release
// the next update installs, with no .prev copy put back by hand. It also fails
// if that section still orders a rollout by hand (the models step, update.sh
// by hand, a step before the merge), if the runbook still has a section that
// starts "## Roll out", or if it still says that the nightly update replaces
// only the binary.
func TestRunbookSaysAReleaseInstallsItsConfigWithItsBinary(t *testing.T) {
	section := runbookSection(t, releaseSection)
	for _, words := range []string{"whole deploy set", "together", "`ghost-wispr.yaml`", "never starts on an old config", "ghost-wispr.local.yaml", "leaving the current release in place", "restored the previous release", "`provision.sh`", "deploy/pi/check-deploy.sh", "revert the change on `main`", "systemctl --user start ghost-wispr-update.service", "Do not restore the `.prev` copies by hand"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's %q does not say %q", releaseSection, words)
		}
	}
	for _, stale := range []string{"--models", "update.sh` by hand", "before you merge"} {
		if strings.Contains(section, stale) {
			t.Errorf("the runbook's %q still orders a rollout by hand: %q", releaseSection, stale)
		}
	}
	runbook := readRunbook(t)
	if strings.Contains(runbook, "\n## Roll out ") {
		t.Error(`the runbook still has a section that starts "## Roll out"`)
	}
	words := strings.Join(strings.Fields(runbook), " ")
	for _, stale := range []string{"replaces only the binary", "replaces the binary alone", "installs only the binary"} {
		if strings.Contains(words, stale) {
			t.Errorf("the runbook still says that the nightly update %s", stale)
		}
	}
}

// pisReleaseSection is the runbook's one section on getting the release the Pi
// runs, which may be older than the latest release.
const pisReleaseSection = "### The release the Pi runs"

// TestRunbookRunsTheModelsStepFromTheReleaseThePiRuns fails if "Enable models
// and Drive sync" does not say to run provision.sh --models only from the
// release the Pi runs, with its block, and that provision.sh --models refuses
// another release's directory; or if "The release the Pi runs" does not say how
// to get that release (check 1 of check-deploy.sh, or the download block of the
// release whose commit /api/version names, which runbook_blocks_test.sh runs),
// or still says that a provisioning change needs that release.
func TestRunbookRunsTheModelsStepFromTheReleaseThePiRuns(t *testing.T) {
	section := runbookSection(t, "## Enable models and Drive sync")
	for _, words := range []string{"only from the release the Pi runs", `"The release the Pi runs"`, "<!-- runbook_blocks_test.sh: models -->", "`provision.sh --models` refuses"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Enable models and Drive sync\" does not say %q", words)
		}
	}
	release := runbookSection(t, pisReleaseSection)
	for _, words := range []string{"check 1", "/api/version", "<!-- runbook_blocks_test.sh: download-commit -->"} {
		if !strings.Contains(release, words) {
			t.Errorf("the runbook's %q does not say %q", pisReleaseSection, words)
		}
	}
	if strings.Contains(release, "a provisioning change run") {
		t.Errorf("the runbook's %q still says that a provisioning change needs the release the Pi runs", pisReleaseSection)
	}
}

// TestRunbookAppliesAProvisioningChangeFromTheReleaseThatCarriesIt fails if
// "Apply a provisioning change" does not run provision.sh from the release that
// carries the change, downloaded as "Provision", step 1 does, or does not say
// what provision.sh does when the Pi runs an older release: it keeps the set the
// Pi runs as its .prev copies, with its line, so a failed install of the newer
// release rolls back to that set; or, when the installed files are not the
// verified set, leaves the .prev copies as they are. It also fails if the
// runbook still says that provision.sh refuses another release, that a
// provisioning change waits until the Pi runs its release, or if "Provision"
// reruns provision.sh in another way than this section's.
func TestRunbookAppliesAProvisioningChangeFromTheReleaseThatCarriesIt(t *testing.T) {
	section := runbookSection(t, "### Apply a provisioning change")
	for _, words := range []string{`("Provision", step 1)`, "kept the set of commit", "rolls back to", "so its .prev copies stay as they are", "`update.sh`, `pi-lib.sh` or `pi.env`", "only from the release the Pi runs"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Apply a provisioning change\" does not say %q", words)
		}
	}
	for _, stale := range []string{"Run it from no other release", "two releases", "refuses when `deploy.verified` names another commit"} {
		if strings.Contains(section, stale) {
			t.Errorf("the runbook's \"Apply a provisioning change\" still says %q", stale)
		}
	}
	if strings.Contains(runbookSection(t, releaseSection), "once the Pi runs that release") {
		t.Errorf("the runbook's %q still waits for the Pi to run a release before its provisioning change", releaseSection)
	}
	provision := runbookSection(t, "## Provision")
	if !strings.Contains(provision, `("Update and inspect", "Apply a provisioning change")`) || strings.Contains(provision, "in the directory of the release the Pi runs") {
		t.Error(`"Provision" does not rerun provision.sh as "Apply a provisioning change" says`)
	}
}

// TestRunbookProvisionGivesServeExactlyServeConf fails if "Provision" does not
// say that provision.sh turns off each Serve mount other than / on a port that
// serve.conf lists, and stops before the join when Serve then holds a part
// serve.conf does not, such as a TCP forward.
func TestRunbookProvisionGivesServeExactlyServeConf(t *testing.T) {
	section := runbookSection(t, "## Provision")
	for _, words := range []string{"each mount other than `/`", "TCP forward", "before the join"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Provision\" does not say %q", words)
		}
	}
}

// TestRunbookSaysHowThePiFindsARenewedCertificate fails if "Renew the X.509
// certificate" does not say that the update keeps the installed certificate's
// version in device.crt.version, with its .prev copy beside device.crt.prev,
// and gives it to pi-secrets, which walks up from it by number when latest
// cannot be read, skips a disabled version and stops at an absent one; or does
// not quote the line pi-secrets prints when that walk finds nothing.
func TestRunbookSaysHowThePiFindsARenewedCertificate(t *testing.T) {
	section := runbookSection(t, "## Renew the X.509 certificate")
	for _, words := range []string{"`/opt/ghost-wispr/device.crt.version`", "`device.crt.version.prev`", "--installed-certificate-version", "skips a disabled", "stops at the first version that does not exist", "pi-secrets: certificate kept: latest certificate cannot be read: <code>; no readable version above <n>"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Renew the X.509 certificate\" does not say %q", words)
		}
	}
}

// TestRunbookSaysWhatTheUpdateRefusesAndRepairs fails if "Update and inspect"
// does not say that the refusal of an override or a drop-in holds for new
// secrets and a renewed certificate too, that the update restarts a recorder
// that started before the installed .env or certificate, and what a rollback
// of an install cut short prints; or if "Turn on the meeting voice" still says
// that the update installs new secrets while an override is set.
func TestRunbookSaysWhatTheUpdateRefusesAndRepairs(t *testing.T) {
	section := runbookSection(t, "## Update and inspect")
	for _, words := range []string{"new secrets or a renewed certificate", "ghost-wispr update: the recorder started before the installed", "restored the last verified set, the one deploy.verified lists"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Update and inspect\" does not say %q", words)
		}
	}
	if strings.Contains(runbookSection(t, "## Turn on the meeting voice"), "still installs new secret values") {
		t.Error(`"Turn on the meeting voice" still says that the nightly update installs new secrets while an override is set`)
	}
}

// TestRunbookSaysWhichSecretValuesThePiRefuses fails if "The Pi's secrets"
// does not say which values pi-secrets refuses, so that a person who adds a
// version knows why the Pi keeps its old one: a value systemd would read
// otherwise than written, with a backslash, a leading quote or white space at
// either end, besides an empty value and a line break or NUL.
func TestRunbookSaysWhichSecretValuesThePiRefuses(t *testing.T) {
	section := runbookSection(t, "## The Pi's secrets")
	for _, words := range []string{"a backslash", "starts with a quote", "white space at either end", "a carriage return, a line feed or a NUL"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"The Pi's secrets\" does not say %q", words)
		}
	}
}

// TestRunbookClaimsTheNameFromAPersonsOwnDevice fails if the runbook still
// runs a step under the agents' secrets broker (secret-run, a broker grant),
// which no person's own device has, or if "Provision" does not claim the
// tailnet name with the block runbook_blocks_test.sh runs.
func TestRunbookClaimsTheNameFromAPersonsOwnDevice(t *testing.T) {
	words := strings.Join(strings.Fields(readRunbook(t)), " ")
	for _, stale := range []string{"secret-run", "secrets-broker", "broker page"} {
		if strings.Contains(words, stale) {
			t.Errorf("the runbook still runs a step under the agents' secrets broker: %q", stale)
		}
	}
	if !strings.Contains(runbookSection(t, "## Provision"), "<!-- runbook_blocks_test.sh: claim-name -->") {
		t.Error(`"Provision" has no claim-name block for runbook_blocks_test.sh`)
	}
}

// TestTheJabraButtonIsOnInTheTemplateAndTheRunbook fails if the office Pi's
// template does not turn the Jabra's Mute button on, or if the template, the
// runbook's section on a release that changes config or "Pause and resume at
// the unit" still keeps the button off until a person's taps pass or until a
// follow-up fix merges, or still says that a provisioning run rewrites .env.
// It also fails if the release section does not say what a release does to
// the Pi's .env override (the nightly update keeps every other line of .env),
// does not check the value the recorder runs with (runbook_blocks_test.sh's
// running-settings block) for jabra_mute_button: true, or checks for the
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
	release := runbookSection(t, releaseSection)
	pause := runbookSection(t, "## Pause and resume at the unit")
	for name, text := range map[string]string{
		"the release section":            release,
		`"Pause and resume at the unit"`: pause,
		"the Pi's config template":       strings.ReplaceAll(strings.Join(strings.Fields(template), " "), "# ", ""),
	} {
		for _, stale := range []string{"real taps", "before the recording light reads red", "stays off either way", "until a person", "follow-up fix", "keeps `jabra_mute_button: false`", "full provisioning run rewrites", "full provisioner rewrites"} {
			if strings.Contains(text, stale) {
				t.Errorf("%s still keeps the Jabra's button off, or says a provisioning run rewrites .env: %q", name, stale)
			}
		}
	}
	for _, words := range []string{"GHOST_WISPR_JABRA_MUTE_BUTTON", "keeps every other line of `.env`", "`jabra_mute_button: true`", "<!-- runbook_blocks_test.sh: running-settings -->"} {
		if !strings.Contains(release, words) {
			t.Errorf("the runbook's %q does not say %q", releaseSection, words)
		}
	}
	if strings.Contains(release, "ends in `false`") {
		t.Errorf("the runbook's %q checks that the Jabra's Mute button is off", releaseSection)
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

// TestRunbookChecksTheMicDeviceAfterARelease fails if the runbook's section on
// a release that changes config does not name mic_device among the keys a
// release changes, or does not check the value the recorder runs with:
// GHOST_WISPR_MIC_DEVICE in the recorder's environment, else the local
// settings file, else the installed config, which runbook_blocks_test.sh's
// running-settings block prints and which must read mic_device: J75. It also
// fails if the section still greps ghost-wispr.yaml alone for mic_device,
// which misses a value saved on the page or set in .env.
func TestRunbookChecksTheMicDeviceAfterARelease(t *testing.T) {
	section := runbookSection(t, releaseSection)
	for _, words := range []string{"`mic_device`", "`mic_device: J75`", "<!-- runbook_blocks_test.sh: running-settings -->", "GHOST_WISPR_MIC_DEVICE", "ghost-wispr.local.yaml"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's %q does not say %q", releaseSection, words)
		}
	}
	if strings.Contains(section, "grep '^mic_device:' /opt/ghost-wispr/ghost-wispr.yaml") {
		t.Errorf("the runbook's %q still reads mic_device from ghost-wispr.yaml alone", releaseSection)
	}
}
