package config

import (
	"strings"
	"testing"
)

// TestHardwareConfigDefaultsAndOverrides fails if hardware controls are enabled
// by accident or an operator cannot override their YAML values.
func TestHardwareConfigDefaultsAndOverrides(t *testing.T) {
	clearEnv(t)
	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if cfg.IndicatorLight != "" {
		t.Fatalf("default indicator light = %q, want disabled", cfg.IndicatorLight)
	}
	if cfg.PowerButton {
		t.Fatal("power button defaults enabled")
	}

	t.Setenv(EnvPrefix+"INDICATOR_LIGHT", "blink1")
	t.Setenv(EnvPrefix+"POWER_BUTTON", "true")
	cfg, _, err = Load("")
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	if cfg.IndicatorLight != "blink1" {
		t.Fatalf("indicator light = %q, want blink1", cfg.IndicatorLight)
	}
	if !cfg.PowerButton {
		t.Fatal("power button override was ignored")
	}

	clearEnv(t)
	cfg, _, err = Load(writeConfig(t, "indicator_light: Blink1\npower_button: true\n"))
	if err != nil {
		t.Fatalf("load YAML hardware settings: %v", err)
	}
	if cfg.IndicatorLight != IndicatorLightBlink1 || !cfg.PowerButton {
		t.Fatalf("YAML hardware settings = (%q, %t), want (blink1, true)", cfg.IndicatorLight, cfg.PowerButton)
	}
}

// TestUnknownIndicatorLightRefusesToStart fails if a set but unknown
// indicator_light lets the recorder start: turning the light check off on a
// typo would let it record with no light on, against the consent rule.
func TestUnknownIndicatorLightRefusesToStart(t *testing.T) {
	clearEnv(t)
	if _, _, err := Load(writeConfig(t, "indicator_light: lantern\n")); err == nil || !strings.Contains(err.Error(), "indicator_light") || !strings.Contains(err.Error(), "lantern") {
		t.Fatalf("Load with indicator_light lantern: err = %v, want a refusal naming indicator_light and the value", err)
	}
	if _, _, err := NewStore(writeConfig(t, "indicator_light: blink-1\n")); err == nil {
		t.Fatal("NewStore accepted indicator_light blink-1")
	}

	clearEnv(t)
	t.Setenv(EnvPrefix+"INDICATOR_LIGHT", "lamp")
	if _, _, err := Load(""); err == nil || !strings.Contains(err.Error(), "lamp") {
		t.Fatalf("Load with %sINDICATOR_LIGHT=lamp: err = %v, want a refusal naming the value", EnvPrefix, err)
	}

	// The two accepted values still load: unset (no light) and blink1.
	clearEnv(t)
	for _, yaml := range []string{"", "indicator_light: \"\"\n", "indicator_light: Blink1\n"} {
		if _, _, err := Load(writeConfig(t, yaml)); err != nil {
			t.Fatalf("Load(%q): %v", yaml, err)
		}
	}
}
