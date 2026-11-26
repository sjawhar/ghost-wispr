package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"DB_PATH", "AUDIO_DIR", "LOG_LEVEL", "SILENCE_TIMEOUT",
		"MIC_SAMPLE_RATE", "MIC_SAMPLE_RATES", "MIC_DEVICE",
		"SUMMARIZATION_MODEL", "GDRIVE_FOLDER_ID", "GOOGLE_CREDENTIALS_FILE",
		"GCP_PROJECT", "GCP_LOCATION", "GENAI_BACKEND", "EMBEDDING_BACKEND", "EMBEDDING_LOCATION", "DEEPGRAM_MODEL",
		"TRANSCRIPTION_MODEL", "BATCH_TRANSCRIPTION_PROVIDER", "BATCH_TRANSCRIPTION_MODEL", "EMBEDDING_MODEL",
		"GDRIVE_SYNC_ENABLED", "GC_ENABLED", "GC_MAX_AGE_DAYS", "GC_MAX_AUDIO_SIZE_MB",
		"INDICATOR_LIGHT", "POWER_BUTTON", "JABRA_MUTE_BUTTON", "PUBLIC_URL", "RECORDING_RULES",
		"ENVOY_ENABLED", "ENVOY_HUB_URL", "ENVOY_WEBHOOK_URL", "ENVOY_WEBHOOK_SECRET", "ENVOY_WEBHOOK_SECRET_NAME",
		"TTS_PROVIDER", "TTS_MODEL", "TTS_VOICE", "TTS_MAX_LENGTH", "TTS_API_KEY",
		"DEEPGRAM_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "CONFIG",
	} {
		t.Setenv(EnvPrefix+key, "")
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
}

func TestBatchRefinementConfigDefaultsAndOverrides(t *testing.T) {
	clearEnv(t)

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.BatchTranscription.Model != "deepgram/nova-3" {
		t.Fatalf("expected the batch model to default to transcription.model deepgram/nova-3, got %q", cfg.BatchTranscription.Model)
	}

	t.Setenv(EnvPrefix+"BATCH_TRANSCRIPTION_MODEL", "deepgram/nova-2")

	cfg, _, err = Load("")
	if err != nil {
		t.Fatalf("Load with env overrides failed: %v", err)
	}
	if cfg.BatchTranscription.Model != "deepgram/nova-2" {
		t.Fatalf("expected env batch model deepgram/nova-2, got %q", cfg.BatchTranscription.Model)
	}
}

func TestSyncAndGCDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.GDriveSyncEnabled {
		t.Error("gdrive_sync_enabled should default to false")
	}
	if cfg.GCEnabled {
		t.Error("gc_enabled should default to false")
	}
	if cfg.GCMaxAgeDays != 30 {
		t.Errorf("expected gc_max_age_days 30, got %d", cfg.GCMaxAgeDays)
	}
	if cfg.GCMaxAudioSizeMB != 1024 {
		t.Errorf("expected gc_max_audio_size_mb 1024, got %d", cfg.GCMaxAudioSizeMB)
	}
}

func TestSyncAndGCEnvOverrides(t *testing.T) {
	t.Setenv(EnvPrefix+"GDRIVE_SYNC_ENABLED", "true")
	t.Setenv(EnvPrefix+"GC_ENABLED", "true")
	t.Setenv(EnvPrefix+"GC_MAX_AGE_DAYS", "60")
	t.Setenv(EnvPrefix+"GC_MAX_AUDIO_SIZE_MB", "512")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GDriveSyncEnabled {
		t.Error("expected gdrive_sync_enabled true")
	}
	if !cfg.GCEnabled {
		t.Error("expected gc_enabled true")
	}
	if cfg.GCMaxAgeDays != 60 {
		t.Errorf("expected gc_max_age_days 60, got %d", cfg.GCMaxAgeDays)
	}
	if cfg.GCMaxAudioSizeMB != 512 {
		t.Errorf("expected gc_max_audio_size_mb 512, got %d", cfg.GCMaxAudioSizeMB)
	}
}

// writeConfig writes a YAML config file and returns its path.
func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// gcWarnings returns the warnings about the GC limits.
func gcWarnings(warnings []string) []string {
	var gc []string
	for _, w := range warnings {
		if strings.Contains(w, "gc_max_") {
			gc = append(gc, w)
		}
	}
	return gc
}

// Each GC limit has a value that means "no limit": 0. Before it did not,
// so stopping the age policy took a large number, and one past 106,751
// days deleted every summarized session. A negative limit is a mistake:
// the default, with a warning.
func TestGCLimitsZeroMeansNoLimit(t *testing.T) {
	clearEnv(t)
	cfg, warnings, err := Load(writeConfig(t, "gc_max_age_days: 0\ngc_max_audio_size_mb: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GCMaxAgeDays != 0 || cfg.GCMaxAudioSizeMB != 0 {
		t.Errorf("gc_max_age_days %d, gc_max_audio_size_mb %d; want 0 and 0, no limit", cfg.GCMaxAgeDays, cfg.GCMaxAudioSizeMB)
	}
	if w := gcWarnings(warnings); len(w) != 0 {
		t.Errorf("no limit warned %q", w)
	}

	cfg, warnings, err = Load(writeConfig(t, "gc_max_age_days: -5\ngc_max_audio_size_mb: -1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GCMaxAgeDays != 30 || cfg.GCMaxAudioSizeMB != 1024 {
		t.Errorf("negative limits gave gc_max_age_days %d, gc_max_audio_size_mb %d; want the defaults, 30 and 1024", cfg.GCMaxAgeDays, cfg.GCMaxAudioSizeMB)
	}
	if w := gcWarnings(warnings); len(w) != 2 {
		t.Errorf("negative limits warned %q, want a warning for each", w)
	}

	t.Setenv(EnvPrefix+"GC_MAX_AGE_DAYS", "0")
	t.Setenv(EnvPrefix+"GC_MAX_AUDIO_SIZE_MB", "0")
	cfg, _, err = Load(writeConfig(t, "gc_max_age_days: 10\ngc_max_audio_size_mb: 20\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GCMaxAgeDays != 0 || cfg.GCMaxAudioSizeMB != 0 {
		t.Errorf("environment 0 gave gc_max_age_days %d, gc_max_audio_size_mb %d; want 0 and 0", cfg.GCMaxAgeDays, cfg.GCMaxAudioSizeMB)
	}
}

// "No limit" set in the settings page survives a restart: the local settings
// file it writes reads back as 0, not as the default.
func TestGCNoLimitSurvivesARestart(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "gc_max_age_days: 30\n")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.GCMaxAgeDays, c.GCMaxAudioSizeMB = 0, 0 }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	restarted, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := restarted.Get(); cfg.GCMaxAgeDays != 0 || cfg.GCMaxAudioSizeMB != 0 {
		t.Errorf("after a restart gc_max_age_days %d, gc_max_audio_size_mb %d; want 0 and 0", cfg.GCMaxAgeDays, cfg.GCMaxAudioSizeMB)
	}
}

// A mutation TryUpdate refuses is rolled back whole: neither the release
// config nor the local settings file changes, and no callback fires, even for
// the fields set before it refused.
func TestTryUpdateRollsBackARefusedMutation(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "gc_max_age_days: 30\n")
	localPath := LocalSettingsPath(path)
	localBefore := []byte("gc_max_audio_size_mb: 512\n")
	if err := os.WriteFile(localPath, localBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	store.OnChange(func(Config) { called = true })

	err = store.TryUpdate(func(c *Config) error {
		c.GCEnabled, c.GCMaxAgeDays = true, 1
		return errors.New("gc.max_audio_size_mb must be 0 (no audio budget) or positive")
	})
	if err == nil {
		t.Fatal("TryUpdate accepted a refused mutation")
	}
	if cfg := store.Get(); cfg.GCEnabled || cfg.GCMaxAgeDays != 30 {
		t.Errorf("refused mutation kept: gc_enabled %v, gc_max_age_days %d", cfg.GCEnabled, cfg.GCMaxAgeDays)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Errorf("refused mutation rewrote the release config:\n%s", after)
	}
	if after, _ := os.ReadFile(localPath); !bytes.Equal(after, localBefore) {
		t.Errorf("refused mutation rewrote the local settings file:\n%s", after)
	}
	if called {
		t.Error("refused mutation fired a change callback")
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
	if cfg.LogLevel != "info" {
		t.Fatalf("expected default log_level, got %q", cfg.LogLevel)
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
	if cfg.Transcription.Model != "deepgram/nova-3" {
		t.Fatalf("expected default transcription.model deepgram/nova-3, got %q", cfg.Transcription.Model)
	}
	if cfg.Embedding.Model != "" || cfg.TTS.Model != "" || cfg.Concierge.Model != "" {
		t.Fatalf("embedding %q, tts %q, concierge %q by default; want each off", cfg.Embedding.Model, cfg.TTS.Model, cfg.Concierge.Model)
	}
}

func TestYAMLLoading(t *testing.T) {
	clearEnv(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
db_path: /custom/db.sqlite
audio_dir: /custom/audio
log_level: debug
silence_timeout: 45s
mic_sample_rate: 48000
mic_sample_rates: [44100, 32000]
summarization:
  model: claude/claude-3-5-sonnet-latest
  presets:
    default:
      description: Team sync summary
      system_prompt: Summarize in bullet points
      user_template: "{{transcript}}"
    concise:
      description: Short summary
      system_prompt: Return three bullets max
      user_template: "Meeting:\n{{transcript}}"
      model: vertex/gemini-2.5-flash
gdrive_folder_id: my-folder
google_credentials_file: /path/to/creds.json
providers:
  claude: {api: anthropic, auth: {kind: api_key, env: EXAMPLE_ANTHROPIC_KEY}}
  vertex: {api: vertex, project: yaml-project, location: europe-west4, auth: {kind: google}}
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
	if cfg.LogLevel != "debug" {
		t.Fatalf("expected yaml log_level, got %q", cfg.LogLevel)
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
	if cfg.Summarization.Model != "claude/claude-3-5-sonnet-latest" {
		t.Fatalf("expected yaml summarization.model, got %q", cfg.Summarization.Model)
	}
	if len(cfg.Summarization.Presets) != 2 {
		t.Fatalf("expected yaml summarization presets, got %#v", cfg.Summarization.Presets)
	}
	if cfg.Summarization.Presets["concise"].Model != "vertex/gemini-2.5-flash" {
		t.Fatalf("expected yaml concise preset model, got %q", cfg.Summarization.Presets["concise"].Model)
	}
	if cfg.GDriveFolderID != "my-folder" {
		t.Fatalf("expected yaml gdrive_folder_id, got %q", cfg.GDriveFolderID)
	}
	if cfg.GoogleCredentialsFile != "/path/to/creds.json" {
		t.Fatalf("expected yaml google_credentials_file, got %q", cfg.GoogleCredentialsFile)
	}
	if v := cfg.Providers["vertex"]; v.Project != "yaml-project" || v.Location != "europe-west4" {
		t.Fatalf("expected the vertex provider's yaml project and location, got %+v", v)
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
	t.Setenv(EnvPrefix+"LOG_LEVEL", "error")

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
	if cfg.LogLevel != "error" {
		t.Fatalf("expected env override for log_level, got %q", cfg.LogLevel)
	}
}

// TestTextToSpeechComesFromTheConfigFile fails if tts.model and tts.voice are
// not read as written.
func TestTextToSpeechComesFromTheConfigFile(t *testing.T) {
	clearEnv(t)
	configPath := writeConfig(t, `
providers:
  vertex-us: {api: vertex, project: example-project, location: us, auth: {kind: google}}
tts:
  model: vertex-us/gemini-3.8-flash-tts
  voice: Kore
`)

	cfg, warnings, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.TTS.Model != "vertex-us/gemini-3.8-flash-tts" || cfg.TTS.Voice != "Kore" || cfg.TTS.MaxLength != 5000 {
		t.Fatalf("tts = %+v, want vertex-us/gemini-3.8-flash-tts, Kore and the 5000 default length", cfg.TTS)
	}
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings: %q", warnings)
	}
}

// TestGoogleCloudTextToSpeechRemainsAProviderForChirp fails if a Chirp 3: HD
// voice on Google Cloud Text-to-Speech, signed in by Google, warns.
func TestGoogleCloudTextToSpeechRemainsAProviderForChirp(t *testing.T) {
	clearEnv(t)
	cfg, warnings, err := Load(writeConfig(t, `
providers:
  cloud-tts: {api: google-tts, auth: {kind: google}}
tts:
  model: cloud-tts/chirp3-hd
  voice: en-US-Chirp3-HD-Charon
`))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if _, err := cfg.ResolveModel(RoleTTS, cfg.TTS.Model); err != nil {
		t.Fatalf("Chirp on Google Cloud Text-to-Speech: %v", err)
	}
	for _, warning := range warnings {
		if strings.Contains(warning, "tts") || strings.Contains(warning, "cloud-tts") {
			t.Fatalf("unexpected Chirp warning: %q", warning)
		}
	}
}

// TestAVertexProviderTakesAServiceAccountsProject fails if a Vertex AI
// provider that names no project does not take the project of a
// service-account credential file, or the global location.
func TestAVertexProviderTakesAServiceAccountsProject(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "service-account.json")
	if err := os.WriteFile(credsPath, []byte(`{"type":"service_account","project_id":"sa-project"}`), 0o644); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credsPath)

	cfg, _, err := Load(writeConfig(t, "providers:\n  vertex: {api: vertex, auth: {kind: google}}\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if v := cfg.Providers["vertex"]; v.Project != "sa-project" || v.Location != "global" {
		t.Fatalf("vertex = %+v, want the service account's project and the global location", v)
	}
}

// TestAVertexProviderWithNoProjectWarnsUnderAnExternalAccount fails if a
// Vertex AI provider takes a project_id from an external-account file, which
// names a pool's project and not the one to bill, or if it then does not warn.
func TestAVertexProviderWithNoProjectWarnsUnderAnExternalAccount(t *testing.T) {
	clearEnv(t)
	credentialsPath := filepath.Join(t.TempDir(), "external-account.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"type":"external_account","project_id":"must-not-be-used"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentialsPath)

	cfg, warnings, err := Load(writeConfig(t, "providers:\n  vertex: {api: vertex, auth: {kind: google}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["vertex"].Project != "" {
		t.Fatalf("vertex took project %q from an external-account file", cfg.Providers["vertex"].Project)
	}
	if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "providers.vertex") && strings.Contains(w, "project") }) {
		t.Fatalf("missing the vertex provider's project warning: %v", warnings)
	}

	_, warnings, err = Load(writeConfig(t, "providers:\n  vertex: {api: vertex, project: example-project, auth: {kind: google}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "providers.vertex") }) {
		t.Fatalf("unexpected vertex warning with its project set: %v", warnings)
	}
}

func TestEnvOverrideSummarizationModel(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"SUMMARIZATION_MODEL", "vertex/gemini-2.5-flash")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Summarization.Model != "vertex/gemini-2.5-flash" {
		t.Fatalf("expected env summarization.model override, got %q", cfg.Summarization.Model)
	}
}

func TestSecretsFromEnvOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "dg-secret")
	t.Setenv(EnvPrefix+"OPENAI_API_KEY", "oai-secret")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if got := cfg.APIKey("deepgram"); got != "dg-secret" {
		t.Fatalf("expected deepgram key from env, got %q", got)
	}
	if got := cfg.APIKey("openai"); got != "oai-secret" {
		t.Fatalf("expected openai key from env, got %q", got)
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

	if len(cfg.ProviderKeys) != 0 {
		t.Fatalf("expected no provider key (yaml should be ignored), got %d", len(cfg.ProviderKeys))
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
		if strings.Contains(w, `"deepgram"`) && strings.Contains(w, "transcription.model") {
			deepgramWarning = true
		}
		if strings.Contains(w, `"openai"`) && strings.Contains(w, "summarization.model") {
			openaiWarning = true
		}
	}

	if !deepgramWarning {
		t.Fatalf("expected a deepgram provider warning when its key is missing, got warnings: %v", warnings)
	}
	if !openaiWarning {
		t.Fatalf("expected an openai provider warning when its key is missing, got warnings: %v", warnings)
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
	t.Setenv(EnvPrefix+"ANTHROPIC_API_KEY", "")
	_, warnings, err := Load(writeConfig(t, `
providers:
  anthropic: {api: anthropic, auth: {kind: api_key, env: GHOST_WISPR_ANTHROPIC_API_KEY}}
summarization:
  model: anthropic/claude-3-5-haiku-latest
transcription:
  model: ""
`))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if !slices.ContainsFunc(warnings, func(w string) bool {
		return strings.Contains(w, `"anthropic"`) && strings.Contains(w, EnvPrefix+"ANTHROPIC_API_KEY")
	}) {
		t.Fatalf("expected an anthropic warning naming its key's variable, got warnings: %v", warnings)
	}

	// A provider only a summarization preset calls needs its key too: the
	// warning names the preset, whose summaries are off.
	_, warnings, err = Load(writeConfig(t, `
providers:
  vertex: {api: vertex, project: vertex-project, auth: {kind: google}}
  anthropic: {api: anthropic, auth: {kind: api_key, env: GHOST_WISPR_ANTHROPIC_API_KEY}}
summarization:
  model: vertex/gemini-example
  presets:
    brief: {model: anthropic/claude-example}
transcription:
  model: ""
`))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !slices.ContainsFunc(warnings, func(w string) bool {
		return strings.Contains(w, `"anthropic"`) && strings.Contains(w, EnvPrefix+"ANTHROPIC_API_KEY") && strings.Contains(w, `summarization preset "brief" model`)
	}) {
		t.Fatalf("expected an anthropic warning naming its key's variable and the preset that calls it, got warnings: %v", warnings)
	}
}

func TestValidationVertexProviderNeedsNoKey(t *testing.T) {
	clearEnv(t)
	_, warnings, err := Load(writeConfig(t, `
providers:
  vertex: {api: vertex, project: vertex-project, auth: {kind: google}}
summarization:
  model: vertex/gemini-3.1-pro-preview
transcription:
  model: ""
`))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings with Vertex AI configured: %v", warnings)
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
