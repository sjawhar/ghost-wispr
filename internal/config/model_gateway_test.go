package config

import (
	"strings"
	"testing"
)

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

// TestAGatewayNeedsNoVertexProjectForTheGeminiVoices fails if a config that
// names a model gateway and no gcp_project is warned that the Gemini TTS
// provider or voice A on Gemini needs a project: through the gateway neither
// does. The same config with no gateway must still be warned, both times.
func TestAGatewayNeedsNoVertexProjectForTheGeminiVoices(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"MODEL_GATEWAY_URL", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	const voices = "speaker_enabled: true\ntts_provider: gemini\nvoice:\n  enabled: true\n  engine: tts\n  tts_engine: gemini\n"
	projectWarnings := func(yaml string) []string {
		t.Helper()
		cfg, warnings, err := Load(writeConfig(t, yaml))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GCPProject != "" {
			t.Fatalf("gcp_project = %q, want none for this test", cfg.GCPProject)
		}
		var named []string
		for _, w := range warnings {
			if strings.Contains(w, "gcp_project") {
				named = append(named, w)
			}
		}
		return named
	}
	if got := projectWarnings(voices); len(got) != 2 {
		t.Errorf("with no gateway and no project, the project warnings are %q, want both voices' two", got)
	}
	if got := projectWarnings(voices + "model_gateway_url: https://model-gateway.example.test\n"); len(got) != 0 {
		t.Errorf("with a gateway and no project, the project warnings are %q, want none", got)
	}
}

// TestAGatewayURLThatIsNotAbsoluteIsWarnedAbout fails unless a
// model_gateway_url with no scheme or no host is warned about at load, and an
// absolute one is not: the calls through it would fail only at first use.
func TestAGatewayURLThatIsNotAbsoluteIsWarnedAbout(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"MODEL_GATEWAY_URL", "")
	warned := func(raw string) bool {
		t.Helper()
		_, warnings, err := Load(writeConfig(t, "model_gateway_url: "+raw+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range warnings {
			if strings.HasPrefix(w, "model_gateway_url ") {
				return true
			}
		}
		return false
	}
	for _, bad := range []string{"model-gateway.example.test", "ftp://model-gateway.example.test", "https://"} {
		if !warned(bad) {
			t.Errorf("model_gateway_url %q loaded with no warning", bad)
		}
	}
	if warned("https://model-gateway.example.test") {
		t.Error("an absolute model_gateway_url was warned about")
	}
}
