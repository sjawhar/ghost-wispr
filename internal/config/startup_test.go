package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestValidateStartup_AllConfigured(t *testing.T) {
	cfg := defaults()
	cfg.ProviderKeys = map[string]string{EnvPrefix + "DEEPGRAM_API_KEY": "dg-1"}
	cfg.GDriveSyncEnabled = true
	cfg.GDriveFolderID = "folder-123"

	status := ValidateStartup(&cfg)

	if status.Deepgram.Status != "connected" {
		t.Errorf("expected deepgram connected, got %q", status.Deepgram.Status)
	}
	if status.DriveSync.Status != "connected" {
		t.Errorf("expected drive sync connected, got %q", status.DriveSync.Status)
	}
	if status.LogLevel != "info" {
		t.Errorf("expected log level info, got %q", status.LogLevel)
	}
}

func TestValidateStartup_MissingDeepgramKey(t *testing.T) {
	cfg := defaults()
	cfg.ProviderKeys = nil

	status := ValidateStartup(&cfg)

	if status.Deepgram.Status != "unavailable" {
		t.Errorf("expected deepgram unavailable, got %q", status.Deepgram.Status)
	}
	if !strings.Contains(status.Deepgram.Message, "transcription unavailable") {
		t.Errorf("expected transcription unavailable message, got %q", status.Deepgram.Message)
	}
}

func TestValidateStartup_DriveSyncDisabled(t *testing.T) {
	cfg := defaults()
	cfg.ProviderKeys = map[string]string{EnvPrefix + "DEEPGRAM_API_KEY": "key"}
	cfg.GDriveSyncEnabled = false

	status := ValidateStartup(&cfg)

	if status.DriveSync.Status != "disabled" {
		t.Errorf("expected drive sync disabled, got %q", status.DriveSync.Status)
	}
}

func TestValidateStartup_DriveSyncEnabledNoFolderID(t *testing.T) {
	cfg := defaults()
	cfg.ProviderKeys = map[string]string{EnvPrefix + "DEEPGRAM_API_KEY": "key"}
	cfg.GDriveSyncEnabled = true
	cfg.GDriveFolderID = ""

	status := ValidateStartup(&cfg)

	if status.DriveSync.Status != "disabled" {
		t.Errorf("expected drive sync disabled when folder ID missing, got %q", status.DriveSync.Status)
	}
	if !strings.Contains(status.DriveSync.Message, "folder ID") {
		t.Errorf("expected message about folder ID, got %q", status.DriveSync.Message)
	}
}

func TestValidateStartup_DriveSyncFullyConfigured(t *testing.T) {
	cfg := defaults()
	cfg.ProviderKeys = map[string]string{EnvPrefix + "DEEPGRAM_API_KEY": "key"}
	cfg.GDriveSyncEnabled = true
	cfg.GDriveFolderID = "folder-abc"

	status := ValidateStartup(&cfg)

	if status.DriveSync.Status != "connected" {
		t.Errorf("expected drive sync connected, got %q", status.DriveSync.Status)
	}
}

func TestValidateStartup_EnabledDisabledLists(t *testing.T) {
	cfg := defaults()
	cfg.ProviderKeys = nil
	cfg.GDriveSyncEnabled = false

	status := ValidateStartup(&cfg)

	enabled := status.Enabled()
	disabled := status.Disabled()

	// deepgram=unavailable, database=ok, mic=connected, sync=disabled
	// enabled should include database and mic (pending init)
	// disabled should include deepgram and sync
	foundDeepgramDisabled := false
	foundSyncDisabled := false
	for _, name := range disabled {
		if name == "deepgram" {
			foundDeepgramDisabled = true
		}
		if name == "sync" {
			foundSyncDisabled = true
		}
	}
	if !foundDeepgramDisabled {
		t.Errorf("expected deepgram in disabled list, got %v", disabled)
	}
	if !foundSyncDisabled {
		t.Errorf("expected sync in disabled list, got %v", disabled)
	}

	foundDBEnabled := false
	foundMicEnabled := false
	for _, name := range enabled {
		if name == "database" {
			foundDBEnabled = true
		}
		if name == "mic" {
			foundMicEnabled = true
		}
	}
	if !foundDBEnabled {
		t.Errorf("expected database in enabled list, got %v", enabled)
	}
	if !foundMicEnabled {
		t.Errorf("expected mic in enabled list, got %v", enabled)
	}
}

// TestAPIKeyStatusShowsNoPartOfTheKey fails if the status the startup log
// gives for the Deepgram key shows any of its characters: the log goes to the
// journal and to anyone who reads it, and the first and last four characters
// of a key narrow the search for it. The status says only "configured" or
// "missing".
func TestAPIKeyStatusShowsNoPartOfTheKey(t *testing.T) {
	for key, want := range map[string]string{
		"":                  "missing",
		"abcdef":            "configured",
		"123456789012":      "configured",
		"dg-abcdefghij-xyz": "configured",
	} {
		if got := APIKeyStatus(key); got != want {
			t.Errorf("APIKeyStatus(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestLogStartupBanner_Output(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	status := StartupStatus{
		Deepgram:  ComponentState{Name: "deepgram", Status: "connected", Message: "ok"},
		Database:  ComponentState{Name: "database", Status: "ok", Message: "ok"},
		Mic:       ComponentState{Name: "mic", Status: "unavailable", Message: "no mic"},
		DriveSync: ComponentState{Name: "sync", Status: "disabled", Message: "not configured"},
		LogLevel:  "debug",
	}

	LogStartupBanner(logger, &status, "1.0.0", "abc123", "2025-01-01")

	output := buf.String()
	if !strings.Contains(output, "startup config summary") {
		t.Errorf("expected startup config summary in log, got %q", output)
	}
	if !strings.Contains(output, "debug") {
		t.Errorf("expected log level in output, got %q", output)
	}
	if !strings.Contains(output, "1.0.0") {
		t.Errorf("expected version in output, got %q", output)
	}
}

func TestLogStartupBanner_AllDisabled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	status := StartupStatus{
		Deepgram:  ComponentState{Name: "deepgram", Status: "unavailable", Message: "no key"},
		Database:  ComponentState{Name: "database", Status: "error", Message: "db failed"},
		Mic:       ComponentState{Name: "mic", Status: "unavailable", Message: "no mic"},
		DriveSync: ComponentState{Name: "sync", Status: "disabled", Message: "not configured"},
		LogLevel:  "info",
	}

	LogStartupBanner(logger, &status, "dev", "unknown", "unknown")

	output := buf.String()
	if !strings.Contains(output, "startup config summary") {
		t.Errorf("expected startup config summary in log, got %q", output)
	}
	if !strings.Contains(output, "unavailable") {
		t.Errorf("expected unavailable in output, got %q", output)
	}
}

func TestComponents_ReturnsAll(t *testing.T) {
	status := StartupStatus{
		Deepgram:  ComponentState{Name: "deepgram", Status: "connected"},
		Database:  ComponentState{Name: "database", Status: "ok"},
		Mic:       ComponentState{Name: "mic", Status: "connected"},
		DriveSync: ComponentState{Name: "sync", Status: "disabled"},
	}

	components := status.Components()
	if len(components) != 4 {
		t.Errorf("expected 4 components, got %d", len(components))
	}
}
