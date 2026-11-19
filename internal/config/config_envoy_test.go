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
	// The repository is public: no deployment's Secret Manager version is a
	// default. The office Pi's config names its own.
	if cfg.EnvoyWebhookSecretName != "" {
		t.Fatalf("default signing key secret = %q, want none", cfg.EnvoyWebhookSecretName)
	}
}

// TestEnvoySigningKeySecretIsNamedNotHeld fails if the configuration takes
// the signing key itself: the recorder reads it from Secret Manager, and the
// configuration only names the secret version.
func TestEnvoySigningKeySecretIsNamedNotHeld(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")
	t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_URL", "https://relay.example.test/webhook/ghostwispr")
	t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_SECRET_NAME", " projects/p/secrets/s/versions/2 ")
	cfg, warnings, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.EnvoyEnabled || cfg.EnvoyWebhookURL != "https://relay.example.test/webhook/ghostwispr" {
		t.Fatalf("Envoy settings = (%t, %q), want the environment's", cfg.EnvoyEnabled, cfg.EnvoyWebhookURL)
	}
	if cfg.EnvoyWebhookSecretName != "projects/p/secrets/s/versions/2" {
		t.Fatalf("signing key secret = %q, want the environment's", cfg.EnvoyWebhookSecretName)
	}
	if text := strings.Join(warnings, "\n"); strings.Contains(text, "Envoy") || strings.Contains(text, "ENVOY") {
		t.Fatalf("warnings = %q, want no Envoy warning: no key needs to be configured", warnings)
	}

	clearEnv(t)
	cfg, _, err = Load(writeConfig(t, "envoy_webhook_secret_name: \" \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnvoyWebhookSecretName != "" {
		t.Fatalf("a blank envoy_webhook_secret_name loaded as %q, want none", cfg.EnvoyWebhookSecretName)
	}
}

// TestEnvoyHubURLNeedsNoWebhookSecret fails if a recorder configured to
// publish on the tailnet hub still requires the public webhook's URL or its
// signing key secret.
func TestEnvoyHubURLNeedsNoWebhookSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")
	t.Setenv(EnvPrefix+"ENVOY_HUB_URL", " http://hub.example.test:9020 ")

	cfg, warnings, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.EnvoyHubURL != "http://hub.example.test:9020" {
		t.Fatalf("hub URL = %q, want trimmed environment value", cfg.EnvoyHubURL)
	}
	if text := strings.Join(warnings, "\n"); strings.Contains(text, "Envoy") || strings.Contains(text, "ENVOY") {
		t.Fatalf("warnings = %q, want no Envoy warning for a hub URL", warnings)
	}
}

// TestALeftoverSigningKeyWarns fails if a signing key an earlier provisioner
// put in the environment or in .env is taken, or left there unmentioned.
func TestALeftoverSigningKeyWarns(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_SECRET", "stale-key")
	_, warnings, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), EnvPrefix+"ENVOY_WEBHOOK_SECRET is no longer read") {
		t.Fatalf("warnings %q do not name the leftover signing key", warnings)
	}

	clearEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte(EnvPrefix+"ENVOY_WEBHOOK_SECRET=stale-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, warnings, err = NewStoreWithEnv(filepath.Join(dir, "ghost-wispr.yaml"), envPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.Join(warnings, "\n"), EnvPrefix+"ENVOY_WEBHOOK_SECRET is no longer read"); got != 1 {
		t.Fatalf("warnings %q name the leftover signing key in .env %d times, want once", warnings, got)
	}
}

func TestEnvoyEnabledWarnings(t *testing.T) {
	t.Run("missing URL", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")

		_, warnings, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), EnvPrefix+"ENVOY_WEBHOOK_URL") {
			t.Fatalf("warnings %q do not name the missing webhook URL", warnings)
		}
	})

	t.Run("missing signing key secret", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")
		t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_URL", "https://relay.example.test/webhook/ghostwispr")

		_, warnings, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), EnvPrefix+"ENVOY_WEBHOOK_SECRET_NAME") {
			t.Fatalf("warnings %q do not name the missing signing key secret", warnings)
		}
	})

	t.Run("non HTTPS URL", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")
		t.Setenv(EnvPrefix+"ENVOY_WEBHOOK_URL", "http://relay.example.test/webhook/ghostwispr")

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
