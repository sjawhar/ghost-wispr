package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"DB_PATH", "AUDIO_DIR", "SILENCE_TIMEOUT",
		"MIC_SAMPLE_RATE", "MIC_SAMPLE_RATES",
		"SUMMARIZATION_MODEL", "GDRIVE_FOLDER_ID", "GOOGLE_CREDENTIALS_FILE",
		"DEEPGRAM_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "CONFIG",
	} {
		t.Setenv(EnvPrefix+key, "")
	}
}

func TestDefaults(t *testing.T) {
	clearEnv(t)

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.DBPath != "data/ghost-wispr.db" {
		t.Fatalf("expected default db_path, got %q", cfg.DBPath)
	}
	if cfg.AudioDir != "data/audio" {
		t.Fatalf("expected default audio_dir, got %q", cfg.AudioDir)
	}
	if cfg.SilenceTimeout != "30s" {
		t.Fatalf("expected default silence_timeout, got %q", cfg.SilenceTimeout)
	}
	if cfg.MicSampleRate != 16000 {
		t.Fatalf("expected default mic_sample_rate 16000, got %d", cfg.MicSampleRate)
	}
	if cfg.Summarization.Model != "openai/gpt-4o-mini" {
		t.Fatalf("expected default summarization.model, got %q", cfg.Summarization.Model)
	}
	preset, ok := cfg.Summarization.Presets["default"]
	if !ok {
		t.Fatal("expected default summarization preset")
	}
	if preset.UserTemplate != "{{transcript}}" {
		t.Fatalf("expected default preset user_template, got %q", preset.UserTemplate)
	}
	if cfg.Transcription.Endpointing != "400" {
		t.Fatalf("expected default transcription.endpointing '400', got %q", cfg.Transcription.Endpointing)
	}
	if cfg.Transcription.UtteranceEndMs != "1000" {
		t.Fatalf("expected default transcription.utterance_end_ms '1000', got %q", cfg.Transcription.UtteranceEndMs)
	}
}

func TestYAMLLoading(t *testing.T) {
	clearEnv(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
db_path: /custom/db.sqlite
audio_dir: /custom/audio
silence_timeout: 45s
mic_sample_rate: 48000
mic_sample_rates: [44100, 32000]
summarization:
  model: anthropic/claude-3-5-sonnet-latest
  presets:
    default:
      description: Team sync summary
      system_prompt: Summarize in bullet points
      user_template: "{{transcript}}"
    concise:
      description: Short summary
      system_prompt: Return three bullets max
      user_template: "Meeting:\n{{transcript}}"
      model: gemini/gemini-2.5-flash
gdrive_folder_id: my-folder
google_credentials_file: /path/to/creds.json
`
	if err := os.WriteFile(configPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.DBPath != "/custom/db.sqlite" {
		t.Fatalf("expected yaml db_path, got %q", cfg.DBPath)
	}
	if cfg.AudioDir != "/custom/audio" {
		t.Fatalf("expected yaml audio_dir, got %q", cfg.AudioDir)
	}
	if cfg.SilenceTimeout != "45s" {
		t.Fatalf("expected yaml silence_timeout, got %q", cfg.SilenceTimeout)
	}
	if cfg.MicSampleRate != 48000 {
		t.Fatalf("expected yaml mic_sample_rate, got %d", cfg.MicSampleRate)
	}
	if !reflect.DeepEqual(cfg.MicSampleRates, []int{44100, 32000}) {
		t.Fatalf("expected yaml mic_sample_rates, got %v", cfg.MicSampleRates)
	}
	if cfg.Summarization.Model != "anthropic/claude-3-5-sonnet-latest" {
		t.Fatalf("expected yaml summarization.model, got %q", cfg.Summarization.Model)
	}
	if len(cfg.Summarization.Presets) != 2 {
		t.Fatalf("expected yaml summarization presets, got %#v", cfg.Summarization.Presets)
	}
	if cfg.Summarization.Presets["concise"].Model != "gemini/gemini-2.5-flash" {
		t.Fatalf("expected yaml concise preset model, got %q", cfg.Summarization.Presets["concise"].Model)
	}
	if cfg.GDriveFolderID != "my-folder" {
		t.Fatalf("expected yaml gdrive_folder_id, got %q", cfg.GDriveFolderID)
	}
	if cfg.GoogleCredentialsFile != "/path/to/creds.json" {
		t.Fatalf("expected yaml google_credentials_file, got %q", cfg.GoogleCredentialsFile)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
db_path: /from/yaml
summarization:
  model: anthropic/claude-3-5-haiku-latest
`
	if err := os.WriteFile(configPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	clearEnv(t)
	t.Setenv(EnvPrefix+"DB_PATH", "/from/env")
	t.Setenv(EnvPrefix+"SUMMARIZATION_MODEL", "openai/gpt-4.1-mini")
	t.Setenv(EnvPrefix+"AUDIO_DIR", "/env/audio")

	cfg, _, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.DBPath != "/from/env" {
		t.Fatalf("expected env override for db_path, got %q", cfg.DBPath)
	}
	if cfg.Summarization.Model != "openai/gpt-4.1-mini" {
		t.Fatalf("expected env override for summarization.model, got %q", cfg.Summarization.Model)
	}
	if cfg.AudioDir != "/env/audio" {
		t.Fatalf("expected env override for audio_dir, got %q", cfg.AudioDir)
	}
}

func TestEnvOverrideSummarizationModel(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"SUMMARIZATION_MODEL", "gemini/gemini-2.5-flash")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Summarization.Model != "gemini/gemini-2.5-flash" {
		t.Fatalf("expected env summarization.model override, got %q", cfg.Summarization.Model)
	}
}

func TestSecretsFromEnvOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "dg-secret")
	t.Setenv(EnvPrefix+"OPENAI_API_KEY", "oai-secret")
	t.Setenv(EnvPrefix+"ANTHROPIC_API_KEY", "anth-secret")
	t.Setenv(EnvPrefix+"GEMINI_API_KEY", "gem-secret")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.DeepgramAPIKey != "dg-secret" {
		t.Fatalf("expected deepgram key from env, got %q", cfg.DeepgramAPIKey)
	}
	if cfg.OpenAIAPIKey != "oai-secret" {
		t.Fatalf("expected openai key from env, got %q", cfg.OpenAIAPIKey)
	}
	if cfg.AnthropicAPIKey != "anth-secret" {
		t.Fatalf("expected anthropic key from env, got %q", cfg.AnthropicAPIKey)
	}
	if cfg.GeminiAPIKey != "gem-secret" {
		t.Fatalf("expected gemini key from env, got %q", cfg.GeminiAPIKey)
	}
}

func TestSecretsIgnoredInYAML(t *testing.T) {
	clearEnv(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
deepgram_api_key: should-be-ignored
openai_api_key: also-ignored
anthropic_api_key: no
gemini_api_key: no
`
	if err := os.WriteFile(configPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.DeepgramAPIKey != "" {
		t.Fatalf("expected empty deepgram key (yaml should be ignored), got %q", cfg.DeepgramAPIKey)
	}
	if cfg.OpenAIAPIKey != "" {
		t.Fatalf("expected empty openai key (yaml should be ignored), got %q", cfg.OpenAIAPIKey)
	}
	if cfg.AnthropicAPIKey != "" {
		t.Fatalf("expected empty anthropic key (yaml should be ignored), got %q", cfg.AnthropicAPIKey)
	}
	if cfg.GeminiAPIKey != "" {
		t.Fatalf("expected empty gemini key (yaml should be ignored), got %q", cfg.GeminiAPIKey)
	}
}

func TestValidationWarnings(t *testing.T) {
	clearEnv(t)

	_, warnings, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	var deepgramWarning, openaiWarning bool
	for _, w := range warnings {
		if strings.Contains(w, "Deepgram") {
			deepgramWarning = true
		}
		if strings.Contains(w, "OpenAI") {
			openaiWarning = true
		}
	}

	if !deepgramWarning {
		t.Fatalf("expected Deepgram warning when key is missing, got warnings: %v", warnings)
	}
	if !openaiWarning {
		t.Fatalf("expected OpenAI warning when key is missing, got warnings: %v", warnings)
	}
}

func TestValidationNoWarningsWhenConfigured(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "key")
	t.Setenv(EnvPrefix+"OPENAI_API_KEY", "key")

	_, warnings, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("expected no warnings when fully configured, got: %v", warnings)
	}
}

func TestValidationWarningsWhenProviderAPIKeyMissing(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "key")
	t.Setenv(EnvPrefix+"SUMMARIZATION_MODEL", "anthropic/claude-3-5-haiku-latest")

	_, warnings, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	var anthropicWarning bool
	for _, w := range warnings {
		if strings.Contains(w, "Anthropic") {
			anthropicWarning = true
			break
		}
	}

	if !anthropicWarning {
		t.Fatalf("expected Anthropic warning when key is missing, got warnings: %v", warnings)
	}
}

func TestInvalidSilenceTimeoutWarning(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "key")
	t.Setenv(EnvPrefix+"OPENAI_API_KEY", "key")
	t.Setenv(EnvPrefix+"SILENCE_TIMEOUT", "not-a-duration")

	cfg, warnings, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if len(warnings) != 1 || !strings.Contains(warnings[0], "silence_timeout") {
		t.Fatalf("expected silence_timeout warning, got: %v", warnings)
	}

	if cfg.ParsedSilenceTimeout() != 30*time.Second {
		t.Fatalf("expected fallback to 30s, got %v", cfg.ParsedSilenceTimeout())
	}
}

func TestMissingConfigFileUsesDefaults(t *testing.T) {
	clearEnv(t)

	cfg, _, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load should not fail for missing config file, got: %v", err)
	}

	if cfg.DBPath != "data/ghost-wispr.db" {
		t.Fatalf("expected defaults when config file missing, got db_path=%q", cfg.DBPath)
	}
}

func TestInvalidConfigFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(configPath, []byte(":::invalid yaml"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	clearEnv(t)

	_, _, err := Load(configPath)
	if err == nil {
		t.Fatal("expected error for invalid yaml, got nil")
	}
}

func TestSampleRateCandidatesDefault(t *testing.T) {
	cfg := defaults()
	got := cfg.SampleRateCandidates()
	want := []int{16000, 48000, 44100, 32000, 24000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected default sample rates: got=%v want=%v", got, want)
	}
}

func TestSampleRateCandidatesCustom(t *testing.T) {
	cfg := defaults()
	cfg.MicSampleRate = 48000
	cfg.MicSampleRates = []int{44100, 16000, 48000, 32000}

	got := cfg.SampleRateCandidates()
	want := []int{48000, 44100, 16000, 32000, 24000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected custom sample rates: got=%v want=%v", got, want)
	}
}

func TestSampleRateCandidatesEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"MIC_SAMPLE_RATE", "48000")
	t.Setenv(EnvPrefix+"MIC_SAMPLE_RATES", "44100,16000,48000,abc,32000")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	got := cfg.SampleRateCandidates()
	want := []int{48000, 44100, 16000, 32000, 24000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected env sample rates: got=%v want=%v", got, want)
	}
}

func TestParseSampleRates(t *testing.T) {
	got := parseSampleRates(" 16000,  ,invalid,0,-1,44100,16000 ")
	want := []int{16000, 44100}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected parsed sample rates: got=%v want=%v", got, want)
	}
}

func TestEnvOverrideSampleRate(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"MIC_SAMPLE_RATE", "48000")
	t.Setenv(EnvPrefix+"MIC_SAMPLE_RATES", "44100,32000")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.MicSampleRate != 48000 {
		t.Fatalf("expected env mic_sample_rate 48000, got %d", cfg.MicSampleRate)
	}
	if !reflect.DeepEqual(cfg.MicSampleRates, []int{44100, 32000}) {
		t.Fatalf("expected env mic_sample_rates, got %v", cfg.MicSampleRates)
	}
}
