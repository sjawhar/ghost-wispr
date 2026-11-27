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
// say that provision.sh makes Serve exactly serve.conf for the device's tailnet
// name: it leaves a Serve that is that already, and otherwise clears Serve
// with tailscale serve reset, which also removes the entry of a name the
// device had before a rename, before it applies each line; that the reset and
// the apply run in a unit of their own, which a kill of provision.sh or a lost
// SSH session does not stop, and which applies serve.conf again when a call
// fails; that the unit reads the recorder's status immediately before the
// reset and refuses, changing nothing, while the recorder is busy; that the
// join runs the same apply; that a Serve left empty is put back only by
// another run of provision.sh, as check 7 says; and that it stops before the
// join when Serve is not serve.conf's then. It also fails if the section still
// says that provision.sh turns parts off one by one, which reaches no other
// name, or that the page is lost for a moment only.
func TestRunbookProvisionGivesServeExactlyServeConf(t *testing.T) {
	section := runbookSection(t, "## Provision")
	for _, words := range []string{"`tailscale serve reset`", "a name the device had before a rename", "a systemd unit of its own", "a lost SSH session", "applies `serve.conf` again", "immediately before the reset", "a call whose `state` is not `none`", "nothing in Serve was changed", "The join runs the same apply", "only another run of `./provision.sh` puts it back", "check 7", "before the join"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Provision\" does not say %q", words)
		}
	}
	for _, stale := range []string{"each mount other than `/`", "a moment that a page open in a browser shows"} {
		if strings.Contains(section, stale) {
			t.Errorf("the runbook's \"Provision\" still says %q", stale)
		}
	}
}

// TestRunbookSaysServeDriftNamesAnotherName fails if "Update and inspect"
// does not say how the drift line shows a Serve entry of another name than
// the device's, such as one a rename left, or that the device's name is read
// with tailscale status --self --json.
func TestRunbookSaysServeDriftNamesAnotherName(t *testing.T) {
	section := runbookSection(t, "## Update and inspect")
	for _, words := range []string{"`<port> <proxy target> on <name>`", "`tailscale status --self --json`"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Update and inspect\" does not say %q", words)
		}
	}
}

// TestRunbookSaysHowThePiFindsARenewedCertificate fails if "Renew the X.509
// certificate" does not say that the update keeps the installed certificate's
// version in device.crt.version, with its .prev copy beside device.crt.prev,
// and gives it to pi-secrets, which walks up from it by number when latest
// cannot be read, skips a disabled version and stops at an absent one; or does
// not quote the line pi-secrets prints when that walk finds nothing. It also
// fails if the section does not give both cases of a failed certificate
// install's rollback: device.crt.version.prev goes back with device.crt.prev,
// or, on a Pi that had no version file before (provision.sh --certificate
// writes none), the new device.crt.version is removed; or if it still says
// that the rollback always puts both .prev copies back.
func TestRunbookSaysHowThePiFindsARenewedCertificate(t *testing.T) {
	section := runbookSection(t, "## Renew the X.509 certificate")
	for _, words := range []string{"`/opt/ghost-wispr/device.crt.version`", "`device.crt.version.prev`", "--installed-certificate-version", "skips a disabled", "stops at the first version that does not exist", "pi-secrets: certificate kept: latest certificate cannot be read: <code>; no readable version above <n>", "puts `device.crt.version.prev` back with it", "it removes the new `device.crt.version`", "`provision.sh --certificate` writes none"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Renew the X.509 certificate\" does not say %q", words)
		}
	}
	if strings.Contains(section, "puts both `.prev` copies back") {
		t.Error(`the runbook's "Renew the X.509 certificate" still says that a rollback always puts both .prev copies back`)
	}
}

// TestRunbookSaysTheCertificatePairChangesAsOne fails if "Renew the X.509
// certificate" does not say how update.sh installs device.crt and
// device.crt.version as one change: the journal device.crt.version.new first,
// then the certificate, then the version file, then the journal's removal, a
// rollback the same way; and what each run, update.sh secrets too, does with a
// journal that a run cut short left, with both lines it prints; and that
// pi-secrets stages no certificate that does not end later than the installed
// device.crt, the comparison it makes, not one with the installed version. It
// also fails if "The Pi's secrets" still says that update.sh secrets changes
// nothing but the four lines of .env, or does not say that it puts right a
// certificate swap cut short, and if "Update and inspect" says a busy recorder
// changes nothing, which the start-of-run repair does change, or does not name
// the two rollbacks that change files while the recorder is busy: a session
// or a call found just before the restart, and a restarted release that fails
// its check while a session runs, whose recorder the rollback does not restart.
func TestRunbookSaysTheCertificatePairChangesAsOne(t *testing.T) {
	section := runbookSection(t, "## Renew the X.509 certificate")
	for _, words := range []string{"`/opt/ghost-wispr/device.crt.version.new`", "Then it renames the certificate over `device.crt`, then writes or removes `device.crt.version`, and then removes the journal", "A rollback puts the pair back the same way", "`update.sh secrets` too", "ghost-wispr update: finished the certificate swap that a run cut short: device.crt.version is <version>", "ghost-wispr update: removed /opt/ghost-wispr/device.crt.version.new, of a run cut short before its certificate went in; device.crt and device.crt.version stay as they are", "stages no certificate that does not end later than the installed `device.crt`"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Renew the X.509 certificate\" does not say %q", words)
		}
	}
	if strings.Contains(section, "not newer than the installed version") {
		t.Error(`the runbook's "Renew the X.509 certificate" says pi-secrets compares a certificate with the installed version; it compares end dates with the installed device.crt`)
	}
	secrets := runbookSection(t, "## The Pi's secrets")
	if strings.Contains(secrets, "writes the four lines and changes nothing else") {
		t.Error(`the runbook's "The Pi's secrets" still says that update.sh secrets changes nothing but the four lines of .env`)
	}
	if !strings.Contains(secrets, "a certificate swap that a run cut short") {
		t.Error(`the runbook's "The Pi's secrets" does not say that update.sh secrets puts right a certificate swap that a run cut short`)
	}
	update := runbookSection(t, "## Update and inspect")
	if strings.Contains(update, "Changes nothing while the recorder is busy") {
		t.Error(`the runbook's "Update and inspect" says a run changes nothing while the recorder is busy; its start-of-run repair can write device.crt.version then`)
	}
	if !strings.Contains(update, "Installs nothing and restarts nothing while the recorder is busy") {
		t.Error(`the runbook's "Update and inspect" does not say that a run installs nothing and restarts nothing while the recorder is busy`)
	}
	if strings.Contains(update, "Only the start of the run above can change a file then") {
		t.Error(`the runbook's "Update and inspect" says only the start of a run changes a file while the recorder is busy; a run that finds a session or a call before its restart puts its swapped files back`)
	}
	for _, words := range []string{"Three things can still change a file while the recorder is busy", "a run whose status read just before the restart finds a session or a call", "a rollback after the restarted release fails its check"} {
		if !strings.Contains(update, words) {
			t.Errorf("the runbook's \"Update and inspect\" does not say %q", words)
		}
	}
}

// TestRunbookSaysTheSecretsHoldThePisOwnValues fails if "The Pi's secrets"
// does not say that each secret's first version was copied once from the Pi's
// own .env; that the blocks run in bash; where a person gets a replacement
// Dispatch token (a no-scope agent token from Dispatch, Settings → Agent
// tokens) and that the form's two ticked boxes must be cleared, since a new
// token gets both scopes unless they are; and that the check's two hashes must
// be equal and neither may be the hash of nothing. It also fails if the
// runbook still mints a token, reads whoami, compares the recorder's start,
// revokes a Dispatch token, or says the form makes a no-scope token by itself.
func TestRunbookSaysTheSecretsHoldThePisOwnValues(t *testing.T) {
	section := runbookSection(t, "## The Pi's secrets")
	for _, words := range []string{"first version was copied once from the Pi's own `.env`", "Run each block in bash", "Settings → Agent tokens", "no-scope agent token", "clear both boxes", "Act as you", "Approve and control credentials", "leave Read only clear", "must be equal", "neither may be `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"The Pi's secrets\" does not say %q", words)
		}
	}
	words := strings.Join(strings.Fields(readRunbook(t)), " ")
	for _, stale := range []string{"/api/v1/me/", "whoami", "mints a new", "mint it into its secret", "DISPATCH_TOKEN_FILE", "revoke the old token", "Revoke the token that the Pi used before", "Dispatch shows a token only in the answer to its mint", "the recorder started at <time>", "Agent tokens, with no scope"} {
		if strings.Contains(words, stale) {
			t.Errorf("the runbook still mints, checks or revokes a Dispatch token, or trusts the form's scopes: %q", stale)
		}
	}
}

// TestRunbookSaysTheBridgeIsReadAgainBeforeTheSwap fails if "Update and
// inspect" does not say that the update compares the call bridge's link
// versions at the first status read and again at the read just before the
// swap, and that a hold at either read changes no file of the deploy set.
func TestRunbookSaysTheBridgeIsReadAgainBeforeTheSwap(t *testing.T) {
	section := runbookSection(t, "## Update and inspect")
	for _, words := range []string{"at the first status read, and again at the status read just before the swap", "A hold at either read changes no file of the deploy set"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Update and inspect\" does not say %q", words)
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

// TestRunbookSaysCheck3FailsAProviderThatDidNotSignInWithNoReport fails if
// "Update and inspect" does not say that check-deploy.sh's check 3 still
// fails for a provider that did not sign in when the recorder's /api/config
// has no local_settings report, which leaves the check UNKNOWN otherwise: a
// reader could take UNKNOWN for all that a recorder with no report gets.
func TestRunbookSaysCheck3FailsAProviderThatDidNotSignInWithNoReport(t *testing.T) {
	section := runbookSection(t, "## Update and inspect")
	for _, words := range []string{"A recorder with no such report gives `UNKNOWN` for checks 2 and 3", "Check 3 still prints `FAIL` for a provider that did not sign in, with or without that report"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Update and inspect\" does not say %q", words)
		}
	}
}

// TestRunbookSaysWhatTheUpdateDoesWhileHeldAndAfterARestart fails if "Update
// and inspect" does not say that a held release still refreshes the secrets
// and the certificate of the release the Pi runs; that the 120 s check after a
// restart needs a status the update can read, besides the commit; that a
// rollback puts the certificate back only when that run swapped it in; that a
// fallback run of update.sh.prev keeps update.sh.prev and pi-lib.sh.prev; and
// that each run removes the staging directories a killed run left. It also
// fails if "A release that changes config" still rolls back on the commit
// alone.
func TestRunbookSaysWhatTheUpdateDoesWhileHeldAndAfterARestart(t *testing.T) {
	section := runbookSection(t, "## Update and inspect")
	for _, words := range []string{"refreshes the secrets and the certificate of the release the Pi runs", "and for `/api/status` to answer a status it can read", "go back only when that run swapped them in", "keeps `update.sh.prev` and `pi-lib.sh.prev` as they are", "older than 46 minutes"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Update and inspect\" does not say %q", words)
		}
	}
	release := runbookSection(t, releaseSection)
	if !strings.Contains(release, "a status the update can read") {
		t.Errorf("the runbook's %q does not say that the new release must answer a status the update can read", releaseSection)
	}
	if strings.Contains(release, "does not answer its commit within 120 s") {
		t.Errorf("the runbook's %q still rolls back on the commit alone", releaseSection)
	}
}

// TestRunbookSaysWhatAPageSaveWrites fails if "Settings saved on the page"
// does not say that a save writes only the settings it changes, never a value
// that .env gives, and drops a setting saved back to the template's value; or
// if the runbook still says that a save writes an .env value into the local
// settings file, where it stays.
func TestRunbookSaysWhatAPageSaveWrites(t *testing.T) {
	section := runbookSection(t, "## Settings saved on the page")
	for _, words := range []string{"A save writes only the settings it changes", "never a value that `.env` gives", "saved back to the template's value leaves the file"} {
		if !strings.Contains(section, words) {
			t.Errorf("the runbook's \"Settings saved on the page\" does not say %q", words)
		}
	}
	if words := strings.Join(strings.Fields(readRunbook(t)), " "); strings.Contains(words, "where it stays after") {
		t.Error("the runbook still says that a save writes an .env value into the local settings file, where it stays")
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
// tailnet name with the block runbook_blocks_test.sh runs, in bash, whose
// read -p zsh reads another way.
func TestRunbookClaimsTheNameFromAPersonsOwnDevice(t *testing.T) {
	words := strings.Join(strings.Fields(readRunbook(t)), " ")
	for _, stale := range []string{"secret-run", "secrets-broker", "broker page"} {
		if strings.Contains(words, stale) {
			t.Errorf("the runbook still runs a step under the agents' secrets broker: %q", stale)
		}
	}
	provision := runbookSection(t, "## Provision")
	if !strings.Contains(provision, "<!-- runbook_blocks_test.sh: claim-name -->") {
		t.Error(`"Provision" has no claim-name block for runbook_blocks_test.sh`)
	}
	if !strings.Contains(provision, "Run the block in bash") {
		t.Error(`"Provision" does not say to run the claim-name block in bash`)
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
