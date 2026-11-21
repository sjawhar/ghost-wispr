package config

import "testing"

// TestModelGatewayURLComesFromTheConfigAndTheEnvironment fails unless
// model_gateway_url is empty by default, is read from the file, and
// GHOST_WISPR_MODEL_GATEWAY_URL, trimmed, replaces the file's value.
func TestModelGatewayURLComesFromTheConfigAndTheEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"MODEL_GATEWAY_URL", "")
	cfg, _, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelGatewayURL != "" {
		t.Errorf("default model_gateway_url = %q, want empty: Vertex AI unless a gateway is named", cfg.ModelGatewayURL)
	}
	path := writeConfig(t, "model_gateway_url: https://from-file.example.test\n")
	if cfg, _, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.ModelGatewayURL != "https://from-file.example.test" {
		t.Errorf("model_gateway_url from the file = %q", cfg.ModelGatewayURL)
	}
	t.Setenv(EnvPrefix+"MODEL_GATEWAY_URL", " https://from-env.example.test ")
	if cfg, _, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.ModelGatewayURL != "https://from-env.example.test" {
		t.Errorf("model_gateway_url with GHOST_WISPR_MODEL_GATEWAY_URL set = %q, want the environment's, trimmed", cfg.ModelGatewayURL)
	}
}
