package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// A settings change does not write GHOST_WISPR_LOG_LEVEL's override into the
// config file: the override stays in memory, and the file keeps its own
// log_level. Another override is written back, as the example config and the
// API spec say of GHOST_WISPR_RECORDING_RULES.
func TestASettingsChangeKeepsTheLogLevelOverrideInMemory(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "log_level: info\naudio_dir: /file/audio\nsilence_timeout: 30s\n")
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
	saved := savedConfig(t, path)
	if saved["log_level"] != "info" || saved["silence_timeout"] != "45s" || saved["audio_dir"] != "/env/audio" {
		t.Errorf("the file says log_level %v, silence_timeout %v, audio_dir %v; want info, 45s, /env/audio", saved["log_level"], saved["silence_timeout"], saved["audio_dir"])
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "50s" }); err != nil {
		t.Fatal(err)
	}
	if saved := savedConfig(t, path); saved["log_level"] != "info" {
		t.Errorf("a second change wrote log_level %v, want info", saved["log_level"])
	}
}

// A settings change that sets the log level itself writes that level, though
// GHOST_WISPR_LOG_LEVEL overrides the file's.
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
	if saved := savedConfig(t, path); saved["log_level"] != "warn" {
		t.Errorf("the file says log_level %v, want warn", saved["log_level"])
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
