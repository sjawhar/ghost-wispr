package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaults_EnvoyConfig(t *testing.T) {
	cfg := defaults()
	if cfg.EnvoyEnabled {
		t.Fatal("expected envoy disabled by default")
	}
	if cfg.EnvoyWebhookURL != "" {
		t.Fatalf("expected no default webhook URL, got %q", cfg.EnvoyWebhookURL)
	}
}

func TestLoad_EnvoyEnvOverrides(t *testing.T) {
	t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")
	t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_URL", "https://relay.example.test/webhook/ghostwispr")
	t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_SECRET", "  shared-secret\n\n")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.EnvoyEnabled {
		t.Fatal("expected envoy enabled from env")
	}
	if cfg.EnvoyWebhookURL != "https://relay.example.test/webhook/ghostwispr" {
		t.Fatalf("expected webhook URL override, got %q", cfg.EnvoyWebhookURL)
	}
	if cfg.EnvoyWebhookSecret != "shared-secret" {
		t.Fatalf("expected listener-compatible whitespace trimming, got %q", cfg.EnvoyWebhookSecret)
	}
}

func TestEnvoyEnabledWarnings(t *testing.T) {
	t.Run("missing secret", func(t *testing.T) {
		t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")
		t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_URL", "https://relay.example.test/webhook/ghostwispr")

		_, warnings, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), EnvPrefix+"ENVOY_WEBHOOK_SECRET") {
			t.Fatalf("warnings %q do not name the missing webhook secret", warnings)
		}
	})

	t.Run("non HTTPS URL", func(t *testing.T) {
		t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")
		t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_URL", "http://relay.example.test/webhook/ghostwispr")
		t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_SECRET", "shared-secret")

		_, warnings, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), "HTTPS") {
			t.Fatalf("warnings %q do not require an HTTPS webhook URL", warnings)
		}
	})
}

func TestOldEnvoyKeysWarn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte("nats_url: nats://broker.example.test:4222\nenvoy_topic: notifications.legacy\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvPrefix+"NATS_URL", "nats://broker.example.test:4222")
	t.Setenv(EnvPrefix+"ENVOY_TOPIC", "notifications.legacy")

	_, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	warningsText := strings.Join(warnings, "\n")
	for _, want := range []string{
		"nats_url", "envoy_topic", "envoy_webhook_url",
		EnvPrefix + "NATS_URL", EnvPrefix + "ENVOY_TOPIC", EnvPrefix + "ENVOY_WEBHOOK_URL",
	} {
		if !strings.Contains(warningsText, want) {
			t.Fatalf("warnings %q do not name %s", warningsText, want)
		}
	}
}
