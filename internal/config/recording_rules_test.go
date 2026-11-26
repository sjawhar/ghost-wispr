package config

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestRecordingRulesAreReadAsWritten fails if recording_rules is not read from
// the YAML file as written and in order, if a unit with none has any, if
// GHOST_WISPR_RECORDING_RULES does not replace them, or if a malformed
// override is dropped without a warning: the web UI shows these rules to the
// people the unit records.
func TestRecordingRulesAreReadAsWritten(t *testing.T) {
	written := []string{
		"Say so before the meeting if you do not want it recorded.",
		"Anyone can pause the recorder, at any time.",
		"Ask the <b>office manager</b> & no one else to delete a segment.",
	}
	yaml := "recording_rules:\n" +
		"  - Say so before the meeting if you do not want it recorded.\n" +
		"  - Anyone can pause the recorder, at any time.\n" +
		"  - \"\"\n" +
		"  - \"Ask the <b>office manager</b> & no one else to delete a segment.\"\n"

	clearEnv(t)
	cfg, _, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.RecordingRules, written) {
		t.Fatalf("recording_rules loaded as %q, want %q (the blank rule left out)", cfg.RecordingRules, written)
	}

	cfg, _, err = Load(writeConfig(t, "silence_timeout: 30s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RecordingRules) != 0 {
		t.Fatalf("a config with no recording_rules loaded %q", cfg.RecordingRules)
	}

	clearEnv(t)
	t.Setenv(EnvPrefix+"RECORDING_RULES", `["Rule one, with a comma.", "Rule two."]`)
	cfg, _, err = Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Rule one, with a comma.", "Rule two."}; !slices.Equal(cfg.RecordingRules, want) {
		t.Fatalf("%sRECORDING_RULES loaded as %q, want %q", EnvPrefix, cfg.RecordingRules, want)
	}

	clearEnv(t)
	t.Setenv(EnvPrefix+"RECORDING_RULES", "Rule one. Rule two.")
	cfg, warnings, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.RecordingRules, written) {
		t.Fatalf("a malformed %sRECORDING_RULES replaced the file's rules with %q", EnvPrefix, cfg.RecordingRules)
	}
	if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, EnvPrefix+"RECORDING_RULES") }) {
		t.Fatalf("a malformed %sRECORDING_RULES gave no warning: %q", EnvPrefix, warnings)
	}
}

// TestASavedConfigKeepsItsRecordingRules fails if a snapshot of the store
// shares the rules with the store, or if a config API save drops them from
// the config a restart reads.
func TestASavedConfigKeepsItsRecordingRules(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "recording_rules:\n  - First rule.\n  - Second rule.\n")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := store.Get()
	snapshot.RecordingRules[0] = "changed in a snapshot"
	if got := store.Get().RecordingRules[0]; got != "First rule." {
		t.Fatalf("a change to a snapshot reached the store: %q", got)
	}

	if err := store.Update(func(c *Config) { c.GCMaxAgeDays = 7 }); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"First rule.", "Second rule."}; !slices.Equal(reloaded.RecordingRules, want) {
		t.Fatalf("after a save the file holds recording_rules %q, want %q", reloaded.RecordingRules, want)
	}
}

// TestASaveWritesTheRulesOverrideIntoTheFile fails if a config API save does
// not write the rules GHOST_WISPR_RECORDING_RULES gives into the local
// settings file, where they stay once the variable is removed, as the README,
// the examples and the OpenAPI spec say. The release config must not change.
func TestASaveWritesTheRulesOverrideIntoTheFile(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "recording_rules:\n  - File rule.\n")
	release, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPrefix+"RECORDING_RULES", `["Override rule."]`)
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.GCMaxAgeDays = 7 }); err != nil {
		t.Fatal(err)
	}

	saved, ok := savedConfig(t, LocalSettingsPath(path))["recording_rules"].([]any)
	if !ok || len(saved) != 1 || saved[0] != "Override rule." {
		t.Errorf("the local settings file holds recording_rules %v, want [Override rule.]", saved)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, release) {
		t.Errorf("a save with the override set changed the release config (%v):\n%s", err, after)
	}
	t.Setenv(EnvPrefix+"RECORDING_RULES", "")
	reloaded, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Override rule."}; !slices.Equal(reloaded.RecordingRules, want) {
		t.Fatalf("after a save with the override set and the variable removed, recording_rules %q, want %q", reloaded.RecordingRules, want)
	}
}
