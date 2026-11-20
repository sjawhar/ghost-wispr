package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func clearVoiceEnv(t *testing.T) {
	t.Helper()
	clearEnv(t)
	for _, key := range []string{
		"VOICE_ENABLED", "VOICE_ENGINE", "VOICE_TTS_ENGINE", "VOICE_TTS_MODEL", "VOICE_TTS_VOICE", "VOICE_TTS_LOCATION",
		"CONCIERGE_MODEL", "CONCIERGE_DISPATCH_URL", "CONCIERGE_ENVOY_URL", "DISPATCH_TOKEN",
	} {
		t.Setenv(EnvPrefix+key, "")
	}
}

// TestVoiceIsOffByDefault fails if a config without a voice section turns the
// voice on, or has no wake phrases, no engine or a listener for when paused.
func TestVoiceIsOffByDefault(t *testing.T) {
	clearVoiceEnv(t)
	cfg, _, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Voice
	if v.Enabled || v.PausedListener {
		t.Errorf("voice enabled %v, paused listener %v by default; want both off", v.Enabled, v.PausedListener)
	}
	if v.Engine != VoiceEngineTTS || v.TTSEngine != TTSEngineGemini || v.TTSLocation != "us" {
		t.Errorf("engine %q, tts engine %q, tts location %q; want tts, gemini, us", v.Engine, v.TTSEngine, v.TTSLocation)
	}
	if !slices.Equal(v.WakePhrases, []string{"Wispr", "Ghost Wispr"}) {
		t.Errorf("wake phrases %q, want Wispr and Ghost Wispr", v.WakePhrases)
	}
}

// TestVoiceAndConciergeComeFromTheConfigAndTheEnvironment fails if the voice
// and Concierge settings are not read from ghost-wispr.yaml, are not
// overridden by their GHOST_WISPR_ variables (an acceptance run turns the
// voice on so), are lost by a settings save, or if the Dispatch token is read
// from the file or written to it.
func TestVoiceAndConciergeComeFromTheConfigAndTheEnvironment(t *testing.T) {
	clearVoiceEnv(t)
	path := writeConfig(t, `
speaker_enabled: true
gcp_project: example-project
voice:
  enabled: true
  engine: tts
  wake_phrases: [Wispr]
  tts_engine: aura
  tts_voice: aura-2-thalia-en
concierge:
  model: gemini-example
  dispatch_url: https://dispatch.example.test
  envoy_url: http://envoy.example.test:9020
dispatch_token: from-the-file
`)
	t.Setenv(EnvPrefix+"DISPATCH_TOKEN", "from-the-env")
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Voice.Enabled || cfg.Voice.TTSEngine != TTSEngineAura || cfg.Voice.TTSVoice != "aura-2-thalia-en" || !slices.Equal(cfg.Voice.WakePhrases, []string{"Wispr"}) {
		t.Errorf("voice %+v, want the file's", cfg.Voice)
	}
	if cfg.Concierge.Model != "gemini-example" || cfg.Concierge.DispatchURL != "https://dispatch.example.test" || cfg.Concierge.EnvoyURL != "http://envoy.example.test:9020" {
		t.Errorf("concierge %+v, want the file's", cfg.Concierge)
	}
	if cfg.DispatchToken != "from-the-env" {
		t.Errorf("dispatch token %q, want the environment's", cfg.DispatchToken)
	}

	t.Setenv(EnvPrefix+"VOICE_ENABLED", "false")
	t.Setenv(EnvPrefix+"VOICE_TTS_ENGINE", "gemini")
	t.Setenv(EnvPrefix+"CONCIERGE_DISPATCH_URL", "https://other.example.test")
	cfg, _, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Voice.Enabled || cfg.Voice.TTSEngine != TTSEngineGemini || cfg.Concierge.DispatchURL != "https://other.example.test" {
		t.Errorf("voice %+v, concierge %+v; want the environment's overrides", cfg.Voice, cfg.Concierge)
	}

	clearVoiceEnv(t)
	t.Setenv(EnvPrefix+"DISPATCH_TOKEN", "from-the-env")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.LogLevel = "debug" }); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tts_engine: aura", "dispatch_url: https://dispatch.example.test"} {
		if !strings.Contains(string(saved), want) {
			t.Errorf("a settings save dropped %q:\n%s", want, saved)
		}
	}
	if strings.Contains(string(saved), "from-the-env") {
		t.Errorf("a settings save wrote the Dispatch token:\n%s", saved)
	}
}

// TestVoiceConfigWarnings fails if a voice that cannot speak or a Concierge
// URL that is not absolute passes without a warning.
func TestVoiceConfigWarnings(t *testing.T) {
	clearVoiceEnv(t)
	cases := map[string]string{
		"no speaker":         "voice:\n  enabled: true\n",
		"unknown engine":     "speaker_enabled: true\nvoice:\n  enabled: true\n  engine: telepathy\n",
		"unknown tts engine": "speaker_enabled: true\nvoice:\n  enabled: true\n  tts_engine: parrot\n",
		"aura with no key":   "speaker_enabled: true\nvoice:\n  enabled: true\n  tts_engine: aura\n",
		"gemini, no project": "speaker_enabled: true\nvoice:\n  enabled: true\n",
		"relative dispatch":  "speaker_enabled: true\ngcp_project: p\nvoice:\n  enabled: true\nconcierge:\n  dispatch_url: dispatch.example.test\n",
		"no wake phrase":     "speaker_enabled: true\ngcp_project: p\nvoice:\n  enabled: true\n  wake_phrases: []\n",
		"listener not built": "speaker_enabled: true\ngcp_project: p\nvoice:\n  enabled: true\n  paused_listener: true\n",
	}
	for name, yaml := range cases {
		_, warnings, err := Load(writeConfig(t, yaml))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, w := range warnings {
			if strings.Contains(strings.ToLower(w), "voice") || strings.Contains(strings.ToLower(w), "concierge") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no voice or concierge warning in %q", name, warnings)
		}
	}
}

// TestVoiceBOpenAIIdentityComesFromTheConfig fails if voice B's two OpenAI
// workload-identity ids (not secrets) are not read from voice.gptlive.
func TestVoiceBOpenAIIdentityComesFromTheConfig(t *testing.T) {
	clearVoiceEnv(t)
	cfg, _, err := Load(writeConfig(t, "voice:\n  gptlive:\n    openai_identity_provider_id: idp-example\n    openai_service_account_id: sa-example\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Voice.GPTLive; got.OpenAIIdentityProviderID != "idp-example" || got.OpenAIServiceAccountID != "sa-example" {
		t.Fatalf("voice.gptlive = %+v, want the file's ids", got)
	}
}
