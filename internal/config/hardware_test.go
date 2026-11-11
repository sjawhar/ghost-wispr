package config

import "testing"

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

func TestInvalidIndicatorLightIsDisabled(t *testing.T) {
	clearEnv(t)
	cfg, warnings, err := Load(writeConfig(t, "indicator_light: lantern\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.IndicatorLight != "" {
		t.Fatalf("invalid indicator light = %q, want disabled", cfg.IndicatorLight)
	}
	if len(warnings) == 0 {
		t.Fatal("invalid indicator light produced no warning")
	}
}
