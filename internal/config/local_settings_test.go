package config

import (
	"bytes"
	"os"
	"path/filepath"
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
// release template: replacing that template would lose the saved timeout and
// leave its new rules unavailable. Before local settings existed, writeYAML
// caused that loss by writing the timeout into the release config itself.
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

	newRelease := []byte("silence_timeout: 30s\nrecording_rules:\n  - New release rule.\nrelease_only_key: new value\n")
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
	if len(cfg.RecordingRules) != 1 || cfg.RecordingRules[0] != "New release rule." {
		t.Errorf("recording_rules after the release replacement = %q, want the new release rule", cfg.RecordingRules)
	}
	gotRelease, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRelease, newRelease) {
		t.Errorf("the page save changed the replacement release config:\n%s", gotRelease)
	}
}

// TestASaveEqualToTheReleaseConfigLeavesNoLocalSettings fails if a no-op page
// save creates a local override that can hide a later release value.
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
