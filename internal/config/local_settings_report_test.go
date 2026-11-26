package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// reportRelease is a release config with one provider, which the local
// settings files of these tests change.
const reportRelease = "silence_timeout: 30s\n" +
	"providers:\n" +
	"  vertex:\n" +
	"    api: vertex\n" +
	"    project: example-project\n" +
	"    auth:\n" +
	"      kind: google\n" +
	"summarization:\n" +
	"  model: vertex/gemini-release\n"

// reportStore opens a store on reportRelease with local, the local settings
// file, beside it; no local settings file when local is empty.
func reportStore(t *testing.T, local string) (*Store, string) {
	t.Helper()
	clearEnv(t)
	path := writeConfig(t, reportRelease)
	if local != "" {
		if err := os.WriteFile(LocalSettingsPath(path), []byte(local), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return store, path
}

// reportJSON is the report's settings as JSON, with its keys sorted.
func reportJSON(t *testing.T, report AppliedLocalSettings) string {
	t.Helper()
	if report.Settings == nil {
		return "null"
	}
	data, err := json.Marshal(report.Settings)
	if err != nil {
		t.Fatalf("the report's settings do not encode as JSON: %v", err)
	}
	return string(data)
}

// TestTheStoreReportsTheLocalSettingsItApplied fails if the store's report of
// the local settings file does not hold the values that it applied from a
// file in YAML's flow style, or holds a provider's sign-in. The acceptance
// driver, deploy/pi/check-deploy.sh, takes the values that the Pi's local
// settings file sets from this report; a shell reader of the file skipped the
// flow style, so a stale model passed check 2 (GPT-6.1 Sol's review of PR
// #201, finding 1). The control: the running config holds the applied values.
func TestTheStoreReportsTheLocalSettingsItApplied(t *testing.T) {
	store, _ := reportStore(t, "summarization: {model: vertex/gemini-saved}\n"+
		"providers: {vertex-eu: {api: vertex, project: example-project, location: europe-west4, auth: {kind: google_id_token, audience: example-audience}}}\n")
	cfg, _, report := store.GetWithLocalSettings()
	if cfg.Summarization.Model != "vertex/gemini-saved" {
		t.Errorf("control: the running summarization model is %q, want the local file's vertex/gemini-saved", cfg.Summarization.Model)
	}
	if got := cfg.Providers["vertex-eu"].Auth.Audience; got != "example-audience" {
		t.Errorf("control: the running vertex-eu provider's audience is %q, want the local file's example-audience", got)
	}
	if report.Err != nil {
		t.Fatalf("the report says the store did not apply the file: %v", report.Err)
	}
	want := `{"providers":{"vertex-eu":{"api":"vertex","location":"europe-west4","project":"example-project"}},"summarization":{"model":"vertex/gemini-saved"}}`
	if got := reportJSON(t, report); got != want {
		t.Errorf("the report holds %s, want %s: the flow-style file's values, with no provider's auth", got, want)
	}
	if strings.Contains(reportJSON(t, report), "audience") {
		t.Error("the report holds a provider's sign-in")
	}
}

// TestTheLocalSettingsReportHoldsOnlyTheSettingsTheConfigReads fails if the
// report holds a key of the local settings file that the config does not read.
// GET /api/config serves the report to every tailnet client, so a key typed
// there by hand, such as provider_keys or dispatch_token, would be served
// though the config never reads it; and an applied: false would make
// deploy/pi/check-deploy.sh read the report as a file not applied. Each
// provider's auth stays out too. The control: the store applied the file, and
// runs its known settings.
func TestTheLocalSettingsReportHoldsOnlyTheSettingsTheConfigReads(t *testing.T) {
	store, _ := reportStore(t, "silence_timeout: 45s\n"+
		"provider_keys: {deepgram: dg-typed-by-hand}\n"+
		"dispatch_token: token-typed-by-hand\n"+
		"applied: false\n"+
		"error: typed by hand\n"+
		"summarization: {model: vertex/gemini-saved, api_key: key-typed-by-hand}\n"+
		"providers: {vertex-eu: {api: vertex, secret: secret-typed-by-hand, labels: {owner: example-owner}, auth: {kind: google}}}\n")
	cfg, _, report := store.GetWithLocalSettings()
	if report.Err != nil {
		t.Fatalf("control: the store did not apply the file: %v", report.Err)
	}
	if cfg.SilenceTimeout != "45s" || cfg.Summarization.Model != "vertex/gemini-saved" {
		t.Errorf("control: the running config has silence_timeout %q and summarization model %q, want the file's", cfg.SilenceTimeout, cfg.Summarization.Model)
	}
	want := `{"providers":{"vertex-eu":{"api":"vertex","labels":{"owner":"example-owner"}}},"silence_timeout":"45s","summarization":{"model":"vertex/gemini-saved"}}`
	if got := reportJSON(t, report); got != want {
		t.Errorf("the report holds %s, want %s: the settings the config reads, with no key it does not read and no provider's auth", got, want)
	}
}

// TestTheLocalSettingsReportIsACopy fails if a change to the report reaches
// the store: GET /api/config hands the report to each request.
func TestTheLocalSettingsReportIsACopy(t *testing.T) {
	store, _ := reportStore(t, "summarization:\n  model: vertex/gemini-saved\n")
	_, _, report := store.GetWithLocalSettings()
	report.Settings["summarization"].(map[string]any)["model"] = "changed in a copy"
	report.Settings["silence_timeout"] = "1s"
	_, _, again := store.GetWithLocalSettings()
	if got := reportJSON(t, again); got != `{"summarization":{"model":"vertex/gemini-saved"}}` {
		t.Errorf("a change to one report reached the next: %s", got)
	}
}

// TestTheLocalSettingsReportNamesAFileTheStoreDidNotApply fails if the store
// reports settings from a local settings file that it did not apply, or does
// not say why; or reports any for no file. The control: the running config is
// the release config's.
func TestTheLocalSettingsReportNamesAFileTheStoreDidNotApply(t *testing.T) {
	store, _ := reportStore(t, "silence_timeout: [\n")
	cfg, _, report := store.GetWithLocalSettings()
	if cfg.SilenceTimeout != "30s" {
		t.Errorf("control: the running silence_timeout is %q, want the release's 30s", cfg.SilenceTimeout)
	}
	if report.Settings != nil || report.Err == nil || !strings.Contains(report.Err.Error(), "yaml:") {
		t.Errorf("the report of a file that does not parse is %s, error %v; want no settings and the parse error", reportJSON(t, report), report.Err)
	}

	none, _ := reportStore(t, "")
	if _, _, report := none.GetWithLocalSettings(); report.Settings != nil || report.Err != nil {
		t.Errorf("the report with no local settings file is %s, error %v; want neither", reportJSON(t, report), report.Err)
	}
}

// TestTheLocalSettingsReportFollowsEachSave fails if the report does not hold
// what a page save changed, or still holds a setting saved back to the release
// config's value, after which no local settings file is left.
func TestTheLocalSettingsReportFollowsEachSave(t *testing.T) {
	store, path := reportStore(t, "")
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}
	if _, _, report := store.GetWithLocalSettings(); reportJSON(t, report) != `{"silence_timeout":"45s"}` {
		t.Errorf("after a save of silence_timeout 45s the report is %s", reportJSON(t, report))
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "30s" }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LocalSettingsPath(path)); !os.IsNotExist(err) {
		t.Fatalf("control: the local settings file is still there after the save back to the release's value (%v)", err)
	}
	if _, _, report := store.GetWithLocalSettings(); report.Settings != nil || report.Err != nil {
		t.Errorf("with no local settings file left the report is %s, error %v; want neither", reportJSON(t, report), report.Err)
	}
}
