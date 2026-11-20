package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaults_EnvoyConfig fails if Envoy is on, or names a hub, by default:
// the repository is public, and each deployment names its own hub.
func TestDefaults_EnvoyConfig(t *testing.T) {
	cfg := defaults()
	if cfg.EnvoyEnabled {
		t.Fatal("expected envoy disabled by default")
	}
	if cfg.EnvoyHubURL != "" {
		t.Fatalf("expected no default hub URL, got %q", cfg.EnvoyHubURL)
	}
}

// TestEnvoyHubURLNeedsNoCredential fails if a recorder configured to publish
// on the tailnet hub is warned about a missing credential: the hub's listener
// takes a tailnet client's publish with none.
func TestEnvoyHubURLNeedsNoCredential(t *testing.T) {
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

	clearEnv(t)
	cfg, _, err = Load(writeConfig(t, "envoy_enabled: true\nenvoy_hub_url: \" http://hub.example.test:9020/ \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EnvoyEnabled || cfg.EnvoyHubURL != "http://hub.example.test:9020/" {
		t.Fatalf("Envoy settings = (%t, %q), want the file's, trimmed", cfg.EnvoyEnabled, cfg.EnvoyHubURL)
	}
}

func TestEnvoyEnabledWarnings(t *testing.T) {
	t.Run("missing hub URL", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")

		_, warnings, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), EnvPrefix+"ENVOY_HUB_URL") {
			t.Fatalf("warnings %q do not name the missing hub URL", warnings)
		}
	})

	t.Run("hub URL that names no host", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")
		t.Setenv(EnvPrefix+"ENVOY_HUB_URL", "hub.example.test:9020")

		_, warnings, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), "absolute URL") {
			t.Fatalf("warnings %q do not require an absolute hub URL", warnings)
		}
	})

	t.Run("control: Envoy off", func(t *testing.T) {
		clearEnv(t)
		_, warnings, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if text := strings.Join(warnings, "\n"); strings.Contains(text, "ENVOY_HUB_URL") {
			t.Fatalf("warnings %q name the hub URL with Envoy off", warnings)
		}
	})
}

// TestRemovedEnvoyKeysWarn fails if a setting of an Envoy path Ghost Wispr no
// longer has is taken silently: a unit configured for NATS or the webhook
// would send nothing and say nothing.
func TestRemovedEnvoyKeysWarn(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "nats_url: nats://broker.example.test:4222\nenvoy_topic: notifications.legacy\n"+
		"envoy_webhook_url: https://relay.example.test/webhook/ghostwispr\nenvoy_webhook_secret_name: projects/p/secrets/s/versions/2\n")
	for _, key := range []string{"NATS_URL", "ENVOY_TOPIC", "ENVOY_WEBHOOK_URL", "ENVOY_WEBHOOK_SECRET_NAME"} {
		t.Setenv(EnvPrefix+key, "set")
	}

	_, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	warningsText := strings.Join(warnings, "\n")
	for _, want := range []string{
		"nats_url has been removed", "envoy_topic has been removed", "envoy_webhook_url has been removed", "envoy_webhook_secret_name has been removed",
		EnvPrefix + "NATS_URL has been removed", EnvPrefix + "ENVOY_TOPIC has been removed",
		EnvPrefix + "ENVOY_WEBHOOK_URL has been removed", EnvPrefix + "ENVOY_WEBHOOK_SECRET_NAME has been removed",
	} {
		if !strings.Contains(warningsText, want) {
			t.Errorf("warnings %q do not say %q", warningsText, want)
		}
	}
	if got := strings.Count(warningsText, "envoy_hub_url"); got != 8 {
		t.Errorf("warnings name envoy_hub_url %d times, want once for each of the 8 removed settings: %q", got, warningsText)
	}
}

// TestALeftoverSigningKeyWarns fails if a webhook signing key an earlier
// provisioner put in the environment or in .env is left there unmentioned.
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
