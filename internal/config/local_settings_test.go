package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	release, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}
	if afterSave, err := os.ReadFile(path); err != nil || !bytes.Equal(afterSave, release) {
		t.Errorf("the page save changed the release config (%v):\n%s", err, afterSave)
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

// fileState is a path as a test compares it before and after a save: absent,
// a directory, or a file with its bytes.
func fileState(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return "absent"
	case err != nil:
		t.Fatal(err)
	case info.IsDir():
		return "a directory"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return "a file holding " + strconv.Quote(string(data))
}

// failedSaveChangedNothing fails t when the failed save err left a trace: no
// error, a revision or a change callback, or a change to the release config or
// the local settings file.
func failedSaveChangedNothing(t *testing.T, store *Store, err error, revision uint64, callbacks int, files map[string]string) {
	t.Helper()
	if err == nil {
		t.Fatal("the save succeeded")
	}
	if _, got := store.GetRevision(); got.Count != revision {
		t.Errorf("the failed save moved the revision from %d to %d", revision, got.Count)
	}
	if callbacks != 0 {
		t.Errorf("the failed save fired %d change callbacks", callbacks)
	}
	for path, before := range files {
		if after := fileState(t, path); after != before {
			t.Errorf("the failed save left %s as %s, want %s", path, after, before)
		}
	}
}

// TestAnEnvFailureRestoresTheLocalSettingsFile fails if a save that cannot
// write .env leaves its own values in the local settings file, as a file that
// was there before or as a new one, touches the release config, moves the
// revision or fires a change callback.
func TestAnEnvFailureRestoresTheLocalSettingsFile(t *testing.T) {
	for name, local := range map[string]string{
		"a local settings file before the save":  "gc_max_age_days: 7\n",
		"no local settings file before the save": "",
	} {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			path := writeConfig(t, "silence_timeout: 30s\n")
			localPath := LocalSettingsPath(path)
			if local != "" {
				if err := os.WriteFile(localPath, []byte(local), 0o644); err != nil {
					t.Fatal(err)
				}
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
			files := map[string]string{path: fileState(t, path), localPath: fileState(t, localPath)}
			_, revision := store.GetRevision()
			callbacks := 0
			store.OnChange(func(Config) { callbacks++ })

			err = store.Update(func(c *Config) {
				c.SilenceTimeout = "45s"
				_ = c.SetAPIKey("openai", "sk-example")
			})
			failedSaveChangedNothing(t, store, err, revision.Count, callbacks, files)
			if got := store.Get(); got.SilenceTimeout != "30s" || got.APIKey("openai") != "" {
				t.Errorf("after the failed save silence_timeout %q, the openai key set: %t; want 30s and no key", got.SilenceTimeout, got.APIKey("openai") != "")
			}
		})
	}
}

// TestALocalSettingsFileFailureChangesNothing fails if a save whose read,
// write or removal of the local settings file fails keeps its change in
// memory, changes a file, moves the revision or fires a change callback.
func TestALocalSettingsFileFailureChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// local is the local settings file the store loads, "" for none.
		local string
		// breakFile makes the save's step on the local settings file fail,
		// after the store loaded.
		breakFile  func(t *testing.T, localPath string)
		save, want string // the silence_timeout the save sets, and the one it must leave
	}{
		{
			name: "read",
			breakFile: func(t *testing.T, localPath string) {
				if err := os.Mkdir(localPath, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			save: "45s", want: "30s",
		},
		{
			name:  "write",
			local: "gc_max_age_days: 7\n",
			// atomicWriteFile writes the file's .tmp first: a directory there
			// fails the write, for root too.
			breakFile: func(t *testing.T, localPath string) {
				if err := os.Mkdir(localPath+".tmp", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(localPath+".tmp", "keep"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			save: "45s", want: "30s",
		},
		{
			name:  "removal",
			local: "silence_timeout: 45s\n",
			// A save of the release's 30s removes the local settings file; a
			// directory that refuses writes refuses the removal.
			breakFile: func(t *testing.T, localPath string) {
				dir := filepath.Dir(localPath)
				if err := os.Chmod(dir, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
				if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o644); err == nil {
					t.Skip("this user writes into a directory with mode 0555 (root), so the removal cannot fail here")
				}
			},
			save: "30s", want: "45s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			path := writeConfig(t, "silence_timeout: 30s\n")
			localPath := LocalSettingsPath(path)
			if tc.local != "" {
				if err := os.WriteFile(localPath, []byte(tc.local), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			store, _, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := store.Get().SilenceTimeout; got != tc.want {
				t.Fatalf("silence_timeout before the save = %q, want %q", got, tc.want)
			}
			tc.breakFile(t, localPath)
			files := map[string]string{path: fileState(t, path), localPath: fileState(t, localPath)}
			_, revision := store.GetRevision()
			callbacks := 0
			store.OnChange(func(Config) { callbacks++ })

			err = store.Update(func(c *Config) { c.SilenceTimeout = tc.save })
			failedSaveChangedNothing(t, store, err, revision.Count, callbacks, files)
			if got := store.Get().SilenceTimeout; got != tc.want {
				t.Errorf("silence_timeout after the failed save = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestASaveOverALocalSettingsFileThatWasNotAppliedIsRefused fails if a save,
// while the local settings file that the start could not apply is in place,
// replaces that file: each setting saved in it would be lost with no message.
// The save must be refused with an error that names the file and what to do.
func TestASaveOverALocalSettingsFileThatWasNotAppliedIsRefused(t *testing.T) {
	for name, local := range map[string]string{
		"does not parse":             "silence_timeout: [\n",
		"a value that does not fit":  "silence_timeout: 45s\ngc_max_age_days: many\n",
		"a null on a struct setting": "silence_timeout: 45s\nvoice: null\n",
	} {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			path := writeConfig(t, "silence_timeout: 30s\n")
			localPath := LocalSettingsPath(path)
			if err := os.WriteFile(localPath, []byte(local), 0o644); err != nil {
				t.Fatal(err)
			}
			store, _, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]string{path: fileState(t, path), localPath: fileState(t, localPath)}
			_, revision := store.GetRevision()
			callbacks := 0
			store.OnChange(func(Config) { callbacks++ })

			err = store.Update(func(c *Config) { c.GCMaxAgeDays = 3 })
			failedSaveChangedNothing(t, store, err, revision.Count, callbacks, files)
			if !strings.Contains(err.Error(), localPath) || !strings.Contains(err.Error(), "restart") {
				t.Errorf("the refusal %q does not name %s and the restart", err, localPath)
			}
			if got := store.Get(); got.GCMaxAgeDays != 30 || got.SilenceTimeout != "30s" {
				t.Errorf("after the refused save gc_max_age_days %d, silence_timeout %q; want the release config's 30 and 30s", got.GCMaxAgeDays, got.SilenceTimeout)
			}
		})
	}
}

// TestAnEmptyRecordingRulesOverrideIsSavedAsNoRules fails if a save made
// while GHOST_WISPR_RECORDING_RULES is [] keeps the release config's rules
// once the variable is removed: an empty list is a value, as R13 says of each
// value from the environment.
func TestAnEmptyRecordingRulesOverrideIsSavedAsNoRules(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "recording_rules:\n  - First release rule.\n  - Second release rule.\n")
	t.Setenv(EnvPrefix+"RECORDING_RULES", "[]")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get().RecordingRules; len(got) != 0 {
		t.Fatalf("recording_rules with %sRECORDING_RULES=[] = %q, want none", EnvPrefix, got)
	}
	if err := store.Update(func(c *Config) { c.GCMaxAgeDays = 7 }); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvPrefix+"RECORDING_RULES", "")
	reopened, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().RecordingRules; len(got) != 0 {
		t.Errorf("recording_rules after the variable is removed = %q, want none", got)
	}
}

// TestAHandWrittenNullOnAStructSettingIsNotApplied fails if a null that a
// person wrote on a setting that is not an entry of a map (providers,
// summarization.presets, a provider's labels) zeroes that setting: the local
// settings file must give a warning that names the setting, and none of it
// applies.
func TestAHandWrittenNullOnAStructSettingIsNotApplied(t *testing.T) {
	for _, tc := range []struct{ local, setting string }{
		{"silence_timeout: 45s\nvoice: null\n", "voice"},
		{"silence_timeout: 45s\nsummarization:\n  presets: null\n", "summarization.presets"},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			clearEnv(t)
			path := writeConfig(t, "silence_timeout: 30s\nspeaker_enabled: true\nvoice:\n  enabled: true\n  engine: dgagent\n")
			localPath := LocalSettingsPath(path)
			if err := os.WriteFile(localPath, []byte(tc.local), 0o644); err != nil {
				t.Fatal(err)
			}

			cfg, warnings, err := Load(path)
			if err != nil {
				t.Fatalf("Load returned an error for a null on %s: %v", tc.setting, err)
			}
			if cfg.SilenceTimeout != "30s" || !cfg.Voice.Enabled || cfg.Voice.Engine != VoiceEngineDGAgent {
				t.Errorf("silence_timeout %q, voice %+v; want the release config's 30s and its voice", cfg.SilenceTimeout, cfg.Voice)
			}
			if _, ok := cfg.Summarization.Presets["default"]; !ok {
				t.Errorf("the presets %v lost the built-in default preset", cfg.Summarization.Presets)
			}
			if !slices.ContainsFunc(warnings, func(w string) bool {
				return strings.HasPrefix(w, localPath+": "+tc.setting+": ") && strings.HasSuffix(w, "; settings saved on the page are not applied")
			}) {
				t.Errorf("warnings %q do not name %s in %s", warnings, tc.setting, localPath)
			}
		})
	}
}

// TestASaveThatClearsAnOmittedSettingKeepsTheLocalSettingsFileApplied fails
// if a save that sets a setting the config file omits when empty
// (deepgram_buffer_size) back to its zero value writes a null for it: a null
// on a setting that is not a map's entry makes the next start skip the whole
// local settings file.
func TestASaveThatClearsAnOmittedSettingKeepsTheLocalSettingsFileApplied(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_BUFFER_SIZE", "")
	path := writeConfig(t, "deepgram_buffer_size: 1000000\n")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.DeepgramBufferSize, c.SilenceTimeout = 0, "45s" }); err != nil {
		t.Fatal(err)
	}

	reopened, warnings, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(warnings, func(w string) bool { return strings.HasPrefix(w, LocalSettingsPath(path)+": ") }) {
		t.Errorf("the start skipped the local settings file the save wrote: %q", warnings)
	}
	if got := reopened.Get(); got.DeepgramBufferSize != 0 || got.SilenceTimeout != "45s" {
		t.Errorf("after a restart deepgram_buffer_size %d, silence_timeout %q; want 0 and 45s", got.DeepgramBufferSize, got.SilenceTimeout)
	}
}

// TestAProviderKeySaveWritesNoProvidersToTheLocalSettingsFile fails if a page
// save of a provider key writes a providers entry to the local settings file:
// the key goes to .env, and a providers entry there would hide what a new
// release config changes in that provider. The save also changes
// silence_timeout, so the local settings file exists and the check reads a
// file the save wrote.
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

// TestAGoogleCredentialsFileOverrideGivesItsOwnVertexProject fails if the
// effective release config holds the Vertex AI project of the release file's
// own credential file: a provider with no project of its own must take the
// project of the credential file it signs in with, and
// GHOST_WISPR_GOOGLE_CREDENTIALS_FILE names another one.
func TestAGoogleCredentialsFileOverrideGivesItsOwnVertexProject(t *testing.T) {
	clearVoiceEnv(t)
	dir := t.TempDir()
	credentials := func(name, project string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, fmt.Appendf(nil, `{"type": "service_account", "project_id": %q}`, project), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	releaseCredentials := credentials("release.json", "release-project")
	overrideCredentials := credentials("override.json", "override-project")
	path := filepath.Join(dir, "ghost-wispr.yaml")
	if err := os.WriteFile(path, fmt.Appendf(nil, "google_credentials_file: %q\nproviders:\n  vertex: {api: vertex, auth: {kind: google}}\n", releaseCredentials), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["vertex"].Project; got != "release-project" {
		t.Fatalf("vertex's project from the release file's credential file = %q, want release-project", got)
	}
	t.Setenv(EnvPrefix+"GOOGLE_CREDENTIALS_FILE", overrideCredentials)
	cfg, _, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GoogleCredentialsFile != overrideCredentials {
		t.Fatalf("google_credentials_file with %sGOOGLE_CREDENTIALS_FILE set = %q, want %q", EnvPrefix, cfg.GoogleCredentialsFile, overrideCredentials)
	}
	if got := cfg.Providers["vertex"].Project; got != "override-project" {
		t.Errorf("vertex's project with %sGOOGLE_CREDENTIALS_FILE set = %q, want override-project, the project of the file it signs in with", EnvPrefix, got)
	}
}

// TestALocalValueThatDoesNotFitItsSettingWarnsByTheLocalFilesLine fails if a
// local settings file whose value does not fit its setting stops the start,
// is applied in part, or gives a warning that names a line of another
// document than the local settings file: here line 4 holds the bad value.
func TestALocalValueThatDoesNotFitItsSettingWarnsByTheLocalFilesLine(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\ngc_max_age_days: 14\n")
	localPath := LocalSettingsPath(path)
	if err := os.WriteFile(localPath, []byte("silence_timeout: 45s\nrecording_rules:\n  - Saved rule.\ngc_max_age_days: many\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned an error for a local value that does not fit its setting: %v", err)
	}
	if cfg.GCMaxAgeDays != 14 || cfg.SilenceTimeout != "30s" || len(cfg.RecordingRules) != 0 {
		t.Errorf("gc_max_age_days %d, silence_timeout %q, recording_rules %q; want the release config's 14, 30s and no rules", cfg.GCMaxAgeDays, cfg.SilenceTimeout, cfg.RecordingRules)
	}
	var local []string
	for _, warning := range warnings {
		if strings.HasPrefix(warning, localPath+": ") {
			local = append(local, warning)
		}
	}
	if len(local) != 1 {
		t.Fatalf("warnings %q, want one about %s", warnings, localPath)
	}
	warning := local[0]
	if !strings.HasSuffix(warning, "; settings saved on the page are not applied") || !strings.Contains(warning, "`many`") {
		t.Errorf("warning %q does not name the bad value and the skipped page saves", warning)
	}
	if !strings.Contains(warning, "line 4:") || strings.Count(warning, "line ") != 1 {
		t.Errorf("warning %q does not name line 4 of the local settings file alone", warning)
	}
}
