package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// A settings change does not write GHOST_WISPR_LOG_LEVEL's override into the
// local settings file: the override stays in memory. Another override is
// written to the local settings file, as the example config and the API spec
// say of GHOST_WISPR_RECORDING_RULES. The release config does not change.
func TestASettingsChangeKeepsTheLogLevelOverrideInMemory(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "log_level: info\naudio_dir: /file/audio\nsilence_timeout: 30s\n")
	release, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPrefix+"LOG_LEVEL", "debug")
	t.Setenv(EnvPrefix+"AUDIO_DIR", "/env/audio")
	store, _, err := NewStoreWithEnv(path, filepath.Join(filepath.Dir(path), ".env"))
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}

	if got := store.Get(); got.LogLevel != "debug" || got.SilenceTimeout != "45s" {
		t.Errorf("in memory: log_level %q, silence_timeout %q; want debug, 45s", got.LogLevel, got.SilenceTimeout)
	}
	saved := savedConfig(t, LocalSettingsPath(path))
	if level, ok := saved["log_level"]; ok || saved["silence_timeout"] != "45s" || saved["audio_dir"] != "/env/audio" {
		t.Errorf("the local settings file says log_level %v (set: %t), silence_timeout %v, audio_dir %v; want no log_level, 45s, /env/audio", level, ok, saved["silence_timeout"], saved["audio_dir"])
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, release) {
		t.Errorf("a settings change changed the release config (%v):\n%s", err, after)
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "50s" }); err != nil {
		t.Fatal(err)
	}
	if level, ok := savedConfig(t, LocalSettingsPath(path))["log_level"]; ok {
		t.Errorf("a second change wrote log_level %v to the local settings file", level)
	}
}

// A settings change that sets the log level itself writes that level to the
// local settings file, though GHOST_WISPR_LOG_LEVEL overrides it in memory.
func TestASettingsChangeOfTheLogLevelWritesIt(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "log_level: info\n")
	t.Setenv(EnvPrefix+"LOG_LEVEL", "debug")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Update(func(c *Config) { c.LogLevel = "warn" }); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "20s" }); err != nil {
		t.Fatal(err)
	}

	if got := store.Get().LogLevel; got != "warn" {
		t.Errorf("in memory: log_level %q, want warn", got)
	}
	if saved := savedConfig(t, LocalSettingsPath(path)); saved["log_level"] != "warn" {
		t.Errorf("the local settings file says log_level %v, want warn", saved["log_level"])
	}
}

func savedConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatalf("parse the saved config: %v", err)
	}
	return saved
}
