package config

import "testing"

func TestDefaults_EnvoyConfig(t *testing.T) {
	cfg := defaults()
	if cfg.EnvoyEnabled {
		t.Fatal("expected envoy disabled by default")
	}
	if cfg.EnvoyTopic != "notifications.ghost-wispr.summary-ready" {
		t.Fatalf("expected default envoy topic, got %q", cfg.EnvoyTopic)
	}
}

func TestLoad_EnvoyEnvOverrides(t *testing.T) {
	t.Setenv(EnvPrefix+"ENVOY_ENABLED", "true")
	t.Setenv(EnvPrefix+"NATS_URL", "nats://127.0.0.1:4222")
	t.Setenv(EnvPrefix+"ENVOY_TOPIC", "notifications.custom.summary-ready")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.EnvoyEnabled {
		t.Fatal("expected envoy enabled from env")
	}
	if cfg.NATSURL != "nats://127.0.0.1:4222" {
		t.Fatalf("expected nats url override, got %q", cfg.NATSURL)
	}
	if cfg.EnvoyTopic != "notifications.custom.summary-ready" {
		t.Fatalf("expected envoy topic override, got %q", cfg.EnvoyTopic)
	}
}
