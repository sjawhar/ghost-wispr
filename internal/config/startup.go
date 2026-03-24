package config

import (
	"log/slog"
	"strings"

	gwstatus "github.com/sjawhar/ghost-wispr/internal/status"
)

// ComponentState represents the startup state of a single component.
type ComponentState struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // "connected", "unavailable", "disabled"
	Message string `json:"message"`
}

// StartupStatus captures the validation results for all components at startup.
type StartupStatus struct {
	Deepgram  ComponentState
	Database  ComponentState
	Mic       ComponentState
	DriveSync ComponentState
	LogLevel  string
}

// Enabled returns the names of enabled/connected components.
func (s StartupStatus) Enabled() []string {
	var enabled []string
	for _, c := range s.Components() {
		if c.Status == gwstatus.ComponentStatusConnected || c.Status == gwstatus.ComponentStatusOK {
			enabled = append(enabled, c.Name)
		}
	}
	return enabled
}

// Disabled returns the names of unavailable/disabled components.
func (s StartupStatus) Disabled() []string {
	var disabled []string
	for _, c := range s.Components() {
		if c.Status != gwstatus.ComponentStatusConnected && c.Status != gwstatus.ComponentStatusOK {
			disabled = append(disabled, c.Name)
		}
	}
	return disabled
}

// Components returns all component states as a slice.
func (s StartupStatus) Components() []ComponentState {
	return []ComponentState{s.Deepgram, s.Database, s.Mic, s.DriveSync}
}

// ValidateStartup checks component readiness based on config values.
// Components that require runtime checks (DB, mic) are set to a pending
// state here and updated later during actual initialization.
func ValidateStartup(cfg Config) StartupStatus {
	status := StartupStatus{
		LogLevel: cfg.LogLevel,
	}

	// Deepgram API key
	if cfg.DeepgramAPIKey == "" {
		status.Deepgram = ComponentState{
			Name:    "deepgram",
			Status:  "unavailable",
			Message: "Deepgram API key not configured — transcription unavailable",
		}
	} else {
		status.Deepgram = ComponentState{
			Name:    "deepgram",
			Status:  gwstatus.ComponentStatusConnected,
			Message: "Deepgram API key present",
		}
	}

	// Database — initialized to pending; main.go sets the real state.
	status.Database = ComponentState{
		Name:    "database",
		Status:  gwstatus.ComponentStatusOK,
		Message: "pending initialization",
	}

	// Mic — initialized to pending; main.go sets the real state.
	status.Mic = ComponentState{
		Name:    "mic",
		Status:  gwstatus.ComponentStatusConnected,
		Message: "pending initialization",
	}

	// Drive sync
	if !cfg.GDriveSyncEnabled {
		status.DriveSync = ComponentState{
			Name:    "sync",
			Status:  "disabled",
			Message: "Drive sync disabled in config",
		}
	} else if cfg.GDriveFolderID == "" {
		status.DriveSync = ComponentState{
			Name:    "sync",
			Status:  "disabled",
			Message: "Drive folder ID not configured — sync disabled",
		}
	} else {
		status.DriveSync = ComponentState{
			Name:    "sync",
			Status:  gwstatus.ComponentStatusConnected,
			Message: "Drive sync enabled",
		}
	}

	return status
}

// MaskAPIKey masks an API key for safe logging. Shows the first 4 and last 4
// characters for keys longer than 12 chars, otherwise just "****".
func MaskAPIKey(key string) string {
	if key == "" {
		return "missing"
	}
	if len(key) <= 12 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

// LogStartupBanner logs a structured startup summary at INFO level.
func LogStartupBanner(logger *slog.Logger, status StartupStatus, version, commit, buildTime string) {
	enabled := status.Enabled()
	disabled := status.Disabled()

	logger.Info("startup config summary",
		"version", version,
		"commit", commit,
		"build_time", buildTime,
		"deepgram", status.Deepgram.Status,
		"database", status.Database.Status,
		"mic", status.Mic.Status,
		"drive_sync", status.DriveSync.Status,
		"log_level", status.LogLevel,
		"enabled_features", strings.Join(enabled, ", "),
		"disabled_features", strings.Join(disabled, ", "),
	)
}
