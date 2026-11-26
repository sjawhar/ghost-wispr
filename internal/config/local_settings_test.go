package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLocalSettingsPath fails if a page save could overwrite a release
// template, or if a non-YAML config's local settings file changed directory.
func TestLocalSettingsPath(t *testing.T) {
	tests := []struct {
		configPath string
		want       string
	}{
		{filepath.Join("var", "lib", "ghost-wispr.yaml"), filepath.Join("var", "lib", "ghost-wispr.local.yaml")},
		{filepath.Join("var", "lib", "ghost-wispr.yml"), filepath.Join("var", "lib", "ghost-wispr.yml.local")},
		{filepath.Join("var", "lib", "ghost-wispr"), filepath.Join("var", "lib", "ghost-wispr.local")},
	}

	for _, test := range tests {
		if got := LocalSettingsPath(test.configPath); got != test.want {
			t.Errorf("LocalSettingsPath(%q) = %q, want %q", test.configPath, got, test.want)
		}
	}
}

// TestAPageSaveSurvivesANewReleaseConfig fails if a settings save rewrites the
// release template: replacing that template would lose the saved timeout, and
// keep the new release's rules and its new gc_max_age_days from the unit.
// Control: a store that writes the whole config into the release file loses
// 45s when the new template replaces it.
func TestAPageSaveSurvivesANewReleaseConfig(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\nrecording_rules:\n  - First release rule.\n")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}

	newRelease := []byte("silence_timeout: 30s\nrecording_rules:\n  - New release rule.\n  - Second new release rule.\ngc_max_age_days: 14\nrelease_only_key: new value\n")
	if err := os.WriteFile(path, newRelease, 0o644); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := reopened.Get()
	if cfg.SilenceTimeout != "45s" {
		t.Errorf("silence_timeout after the release replacement = %q, want 45s", cfg.SilenceTimeout)
	}
	if want := []string{"New release rule.", "Second new release rule."}; !slices.Equal(cfg.RecordingRules, want) {
		t.Errorf("recording_rules after the release replacement = %q, want %q", cfg.RecordingRules, want)
	}
	if cfg.GCMaxAgeDays != 14 {
		t.Errorf("gc_max_age_days after the release replacement = %d, want the new release's 14", cfg.GCMaxAgeDays)
	}
	gotRelease, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRelease, newRelease) {
		t.Errorf("the page save changed the replacement release config:\n%s", gotRelease)
	}
}

// TestAPageSaveOfABuiltInPresetKeepsItsOtherFields fails if a save that
// changes one field of the built-in default preset, which the release config
// does not name, loses the preset's other fields at the next start: its user
// template, which carries the transcript, and its description.
func TestAPageSaveOfABuiltInPresetKeepsItsOtherFields(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\n")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) {
		preset := c.Summarization.Presets["default"]
		preset.SystemPrompt = "Give the decisions first."
		c.Summarization.Presets["default"] = preset
	}); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Preset{
		Description:  "General-purpose summary adapted to content depth",
		SystemPrompt: "Give the decisions first.",
		UserTemplate: "{{transcript}}",
	}
	if got, ok := reopened.Get().Summarization.Presets["default"]; !ok || got != want {
		t.Errorf("the default preset after a restart = %+v (held: %t), want %+v", got, ok, want)
	}
}

// TestAPageSaveOfAPresetFieldKeepsANewReleasesOtherFields fails if a save
// that changes one field of a preset writes the preset's other fields too:
// those fields then hide what a new release config changes in them, as here
// the new release's user template. Maps merge key by key, presets too.
func TestAPageSaveOfAPresetFieldKeepsANewReleasesOtherFields(t *testing.T) {
	clearEnv(t)
	const preset = "summarization:\n  presets:\n    standup:\n      description: Stand-up\n      system_prompt: Summarize briefly\n      user_template: \"%s: {{transcript}}\"\n"
	path := writeConfig(t, fmt.Sprintf(preset, "Old"))
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) {
		standup := c.Summarization.Presets["standup"]
		standup.SystemPrompt = "Give the decisions first."
		c.Summarization.Presets["standup"] = standup
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fmt.Appendf(nil, preset, "New"), 0o644); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Preset{Description: "Stand-up", SystemPrompt: "Give the decisions first.", UserTemplate: "New: {{transcript}}"}
	if got, ok := reopened.Get().Summarization.Presets["standup"]; !ok || got != want {
		t.Errorf("the standup preset after a new release = %+v (held: %t), want %+v", got, ok, want)
	}
}

// TestADeletedBuiltInPresetStaysDeleted fails if the built-in default preset,
// deleted on the page, comes back at the next start because the defaults hold
// it. The same save adds a standup preset: it must be there after the
// restart, so the check reads a store that applied the local settings file.
func TestADeletedBuiltInPresetStaysDeleted(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\n")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get().Summarization.Presets["default"]; !ok {
		t.Fatal("the store holds no built-in default preset to delete")
	}
	if err := store.Update(func(c *Config) {
		delete(c.Summarization.Presets, "default")
		c.Summarization.Presets["standup"] = Preset{Description: "Stand-up", UserTemplate: "{{transcript}}"}
	}); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	presets := reopened.Get().Summarization.Presets
	if _, ok := presets["standup"]; !ok {
		t.Fatalf("after a restart the presets are %v: the saved standup preset is missing", presets)
	}
	if preset, ok := presets["default"]; ok {
		t.Errorf("after a restart the deleted default preset is back: %+v", preset)
	}
}

// TestAPageSaveComparesWithTheReleaseConfigTheStoreLoaded fails if a save
// made after a new release config replaced the file under a running store
// compares with the file on disk: each setting the new release changed then
// differs from the running config, and the save writes the old release's
// value as a page save, which hides the new release's value from then on.
func TestAPageSaveComparesWithTheReleaseConfigTheStoreLoaded(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\ngc_max_age_days: 30\n")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("silence_timeout: 30s\ngc_max_age_days: 60\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}

	local := savedConfig(t, LocalSettingsPath(path))
	if value, ok := local["gc_max_age_days"]; ok {
		t.Errorf("the save wrote the old release's gc_max_age_days %v to the local settings file", value)
	}
	if local["silence_timeout"] != "45s" {
		t.Errorf("the local settings file holds silence_timeout %v, want 45s", local["silence_timeout"])
	}
	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := reopened.Get(); cfg.GCMaxAgeDays != 60 || cfg.SilenceTimeout != "45s" {
		t.Errorf("after a restart gc_max_age_days %d, silence_timeout %q; want the new release's 60, and 45s", cfg.GCMaxAgeDays, cfg.SilenceTimeout)
	}
}

// TestASaveEqualToTheReleaseConfigLeavesNoLocalSettings fails if a no-op page
// save creates a local override that can hide a later release value, or if a
// save that sets the last saved setting back to the release's value leaves the
// local settings file in place.
func TestASaveEqualToTheReleaseConfigLeavesNoLocalSettings(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\n")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "30s" }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LocalSettingsPath(path)); !os.IsNotExist(err) {
		t.Fatalf("an equal page save created local settings: %v", err)
	}

	// A local settings file that exists: the store reads 45s from it, and a
	// save of the release's 30s leaves no difference.
	if err := os.WriteFile(LocalSettingsPath(path), []byte("silence_timeout: 45s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().SilenceTimeout; got != "45s" {
		t.Fatalf("silence_timeout from the local settings file = %q, want 45s", got)
	}
	if err := reopened.Update(func(c *Config) { c.SilenceTimeout = "30s" }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LocalSettingsPath(path)); !os.IsNotExist(err) {
		t.Fatalf("a save equal to the release config left the local settings file: %v", err)
	}
}

// TestAFalseSavedOverATrueTemplateStays fails if the local diff mistakes
// false for an absent value and allows the release template's true back in.
func TestAFalseSavedOverATrueTemplateStays(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "jabra_mute_button: true\n")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.JabraMuteButton = false }); err != nil {
		t.Fatal(err)
	}
	if got, ok := savedConfig(t, LocalSettingsPath(path))["jabra_mute_button"]; !ok || got != false {
		t.Errorf("local jabra_mute_button = %v, want false", got)
	}
	gotRelease, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRelease, original) {
		t.Errorf("the false page save changed the release config:\n%s", gotRelease)
	}
	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().JabraMuteButton; got {
		t.Errorf("jabra_mute_button after reopen = %t, want false", got)
	}
}

// TestAnEnvironmentOverrideIsSavedToTheLocalSettingsFile fails if a page save
// writes an environment override into the release config or loses it when the
// override is removed before the next start.
func TestAnEnvironmentOverrideIsSavedToTheLocalSettingsFile(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "recording_rules:\n  - Release rule.\n")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPrefix+"RECORDING_RULES", `["Environment rule."]`)
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.GCMaxAgeDays = 7 }); err != nil {
		t.Fatal(err)
	}
	localRules, ok := savedConfig(t, LocalSettingsPath(path))["recording_rules"].([]any)
	if !ok || len(localRules) != 1 || localRules[0] != "Environment rule." {
		t.Errorf("local recording_rules = %v, want [Environment rule.]", localRules)
	}
	gotRelease, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRelease, original) {
		t.Errorf("the environment override changed the release config:\n%s", gotRelease)
	}

	t.Setenv(EnvPrefix+"RECORDING_RULES", "")
	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rules := reopened.Get().RecordingRules
	if len(rules) != 1 || rules[0] != "Environment rule." {
		t.Errorf("recording_rules after removing the environment override = %q, want [Environment rule.]", rules)
	}
}

// TestTheLogLevelOverrideIsNeverSaved fails if GHOST_WISPR_LOG_LEVEL escapes
// a single process and changes the release or local settings file.
func TestTheLogLevelOverrideIsNeverSaved(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "log_level: info\nsilence_timeout: 30s\n")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPrefix+"LOG_LEVEL", "debug")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}
	if _, ok := savedConfig(t, LocalSettingsPath(path))["log_level"]; ok {
		t.Error("a GHOST_WISPR_LOG_LEVEL override reached local settings")
	}
	gotRelease, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRelease, original) {
		t.Errorf("the log-level override changed the release config:\n%s", gotRelease)
	}
}

// TestABrokenLocalSettingsFileWarnsAndUsesTheReleaseConfig fails if one bad
// page-save file prevents the release configuration from starting the Pi or
// hides the warning that tells an operator the page save was skipped.
func TestABrokenLocalSettingsFileWarnsAndUsesTheReleaseConfig(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\n")
	localPath := LocalSettingsPath(path)
	if err := os.WriteFile(localPath, []byte("silence_timeout: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned the broken local settings error instead of the release config: %v", err)
	}
	if cfg.SilenceTimeout != "30s" {
		t.Errorf("silence_timeout with broken local settings = %q, want 30s from the release config", cfg.SilenceTimeout)
	}
	wantSuffix := "; settings saved on the page are not applied"
	for _, warning := range warnings {
		if strings.HasPrefix(warning, localPath+": ") && strings.HasSuffix(warning, wantSuffix) {
			return
		}
	}
	t.Errorf("warnings %q do not name the broken local settings file and skipped page saves", warnings)
}

// TestAnEnvFailureRestoresTheLocalSettingsFile fails if a save that cannot
// write .env leaves its own values in the local settings file, or touches the
// release config while it rolls back.
func TestAnEnvFailureRestoresTheLocalSettingsFile(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\n")
	release, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	localPath := LocalSettingsPath(path)
	localBefore := []byte("gc_max_age_days: 7\n")
	if err := os.WriteFile(localPath, localBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory where .env must be: the rename of the new .env over it fails.
	envPath := filepath.Join(filepath.Dir(path), ".env")
	if err := os.Mkdir(envPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envPath, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}

	err = store.Update(func(c *Config) {
		c.SilenceTimeout = "45s"
		_ = c.SetAPIKey("openai", "sk-example")
	})
	if err == nil {
		t.Fatal("a save whose .env write failed succeeded")
	}
	if got := store.Get().SilenceTimeout; got != "30s" {
		t.Errorf("silence_timeout after the failed save = %q, want 30s", got)
	}
	if after, _ := os.ReadFile(localPath); !bytes.Equal(after, localBefore) {
		t.Errorf("the failed save left the local settings file as:\n%s\nwant:\n%s", after, localBefore)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, release) {
		t.Errorf("the failed save changed the release config:\n%s", after)
	}
}

// TestAProviderKeySaveWritesNoProvidersToTheLocalSettingsFile fails if a page
// save of a provider key writes a providers entry to the local settings file:
// the key goes to .env, and a providers entry there would replace the release
// config's provider from then on. The save also changes silence_timeout, so
// the local settings file exists and the check reads a file the save wrote.
func TestAProviderKeySaveWritesNoProvidersToTheLocalSettingsFile(t *testing.T) {
	clearVoiceEnv(t)
	dir := t.TempDir()
	path, envPath := filepath.Join(dir, "ghost-wispr.yaml"), filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(examplePi), 0o644); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) {
		c.SilenceTimeout = "45s"
		if err := c.SetAPIKey("deepgram", "dg-saved"); err != nil {
			t.Errorf("SetAPIKey(deepgram): %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}

	local := savedConfig(t, LocalSettingsPath(path))
	if local["silence_timeout"] != "45s" {
		t.Errorf("the local settings file holds silence_timeout %v, want 45s", local["silence_timeout"])
	}
	if providers, ok := local["providers"]; ok {
		t.Errorf("a provider key save wrote providers to the local settings file: %v", providers)
	}
	if data, _ := os.ReadFile(LocalSettingsPath(path)); strings.Contains(string(data), "dg-saved") {
		t.Errorf("the local settings file holds the saved key:\n%s", data)
	}
}

// TestAProjectFromGoogleApplicationCredentialsIsSavedLikeAnEnvironmentOverride
// fails if the release config a save compares with reads
// GOOGLE_APPLICATION_CREDENTIALS. That release config has no environment, so
// the Vertex AI project the variable's credential file gives is a value from
// the environment: a save writes it to the local settings file, and it stays
// after the variable is removed.
func TestAProjectFromGoogleApplicationCredentialsIsSavedLikeAnEnvironmentOverride(t *testing.T) {
	clearVoiceEnv(t)
	dir := t.TempDir()
	credentials := filepath.Join(dir, "service-account.json")
	if err := os.WriteFile(credentials, []byte(`{"type": "service_account", "project_id": "env-project"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentials)
	path := filepath.Join(dir, "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte("providers:\n  vertex: {api: vertex, auth: {kind: google}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get().Providers["vertex"].Project; got != "env-project" {
		t.Fatalf("vertex's project with GOOGLE_APPLICATION_CREDENTIALS set = %q, want env-project: the check below could not see the variable's project", got)
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}

	providers, _ := savedConfig(t, LocalSettingsPath(path))["providers"].(map[string]any)
	vertex, _ := providers["vertex"].(map[string]any)
	if vertex["project"] != "env-project" {
		t.Errorf("the local settings file holds providers %v, want vertex's project env-project", providers)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().Providers["vertex"].Project; got != "env-project" {
		t.Errorf("vertex's project after the variable is removed = %q, want env-project", got)
	}
}
