package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/llm"

	"gopkg.in/yaml.v3"
)

// EnvPrefix is the namespace prefix for all Ghost Wispr environment variables.
const EnvPrefix = "GHOST_WISPR_"

// Config holds all application configuration. Secrets (API keys) are loaded
// exclusively from environment variables and never appear in the config file.
type Preset struct {
	Description  string `yaml:"description" json:"description"`
	SystemPrompt string `yaml:"system_prompt" json:"system_prompt"`
	UserTemplate string `yaml:"user_template" json:"user_template"`
	Model        string `yaml:"model" json:"model"`
}

type Summarization struct {
	Model           string            `yaml:"model"`
	BaseURL         string            `yaml:"base_url"`
	Presets         map[string]Preset `yaml:"presets"`
	MinSummaryWords int               `yaml:"min_summary_words"`
}

type Transcription struct {
	Endpointing    string   `yaml:"endpointing"`
	UtteranceEndMs string   `yaml:"utterance_end_ms"`
	Keywords       []string `yaml:"keywords"`
}

type BatchTranscription struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type Config struct {
	DBPath                        string             `yaml:"db_path"`
	AudioDir                      string             `yaml:"audio_dir"`
	LogLevel                      string             `yaml:"log_level"`
	SilenceTimeout                string             `yaml:"silence_timeout"`
	MinSessionSegments            int                `yaml:"min_session_segments"`
	MicSampleRate                 int                `yaml:"mic_sample_rate"`
	MicSampleRates                []int              `yaml:"mic_sample_rates"`
	GDriveFolderID                string             `yaml:"gdrive_folder_id"`
	GoogleCredentialsFile         string             `yaml:"google_credentials_file"`
	GCPProject                    string             `yaml:"gcp_project"`
	GCPLocation                   string             `yaml:"gcp_location"`
	GenAIBackend                  string             `yaml:"genai_backend"`
	GDriveSyncEnabled             bool               `yaml:"gdrive_sync_enabled"`
	EnvoyEnabled                  bool               `yaml:"envoy_enabled"`
	NATSURL                       string             `yaml:"nats_url"`
	EnvoyTopic                    string             `yaml:"envoy_topic"`
	GCEnabled                     bool               `yaml:"gc_enabled"`
	GCMaxAgeDays                  int                `yaml:"gc_max_age_days"`
	GCMaxAudioSizeMB              int                `yaml:"gc_max_audio_size_mb"`
	Summarization                 Summarization      `yaml:"summarization"`
	Transcription                 Transcription      `yaml:"transcription"`
	BatchTranscription            BatchTranscription `yaml:"batch_transcription"`
	EmbeddingModel                string             `yaml:"embedding_model"`
	DeepgramModel                 string             `yaml:"deepgram_model"`
	DeepgramBufferSize            int                `yaml:"deepgram_buffer_size"`
	DeepgramReconnectInitialDelay string             `yaml:"deepgram_reconnect_initial_delay"`
	DeepgramReconnectMaxBackoff   string             `yaml:"deepgram_reconnect_max_backoff"`
	SpeakerEnabled                bool               `yaml:"speaker_enabled"`
	SpeakerDevice                 string             `yaml:"speaker_device"`
	TTSProvider                   string             `yaml:"tts_provider"`
	TTSVoice                      string             `yaml:"tts_voice"`
	TTSMaxLength                  int                `yaml:"tts_max_length"`

	// Secrets — env vars only, never serialized to YAML.
	DeepgramAPIKey  string `yaml:"-"`
	OpenAIAPIKey    string `yaml:"-"`
	AnthropicAPIKey string `yaml:"-"`
	GeminiAPIKey    string `yaml:"-"`
	TTSAPIKey       string `yaml:"-"`
}

func defaults() Config {
	return Config{
		DBPath:                "data/ghost-wispr.db",
		AudioDir:              "data/audio",
		LogLevel:              "info",
		SilenceTimeout:        "30s",
		MinSessionSegments:    0,
		MicSampleRate:         16000,
		MicSampleRates:        []int{48000, 44100, 32000, 24000},
		GoogleCredentialsFile: "./service-account.json",
		GCPLocation:           genaiconfig.DefaultLocation,
		EnvoyTopic:            "notifications.ghost-wispr.summary-ready",
		GCMaxAgeDays:          30,
		GCMaxAudioSizeMB:      1024,
		Summarization: Summarization{
			Model:           "openai/gpt-4o-mini",
			MinSummaryWords: 20,
			Presets: map[string]Preset{
				"default": {
					Description:  "General-purpose summary adapted to content depth",
					SystemPrompt: "",
					UserTemplate: "{{transcript}}",
				},
			},
		},
		Transcription: Transcription{
			Endpointing:    "400",
			UtteranceEndMs: "1000",
		},
		BatchTranscription: BatchTranscription{
			Provider: "deepgram",
			Model:    "nova-3",
		},
		EmbeddingModel:                "",
		DeepgramModel:                 "nova-3",
		DeepgramBufferSize:            1920000,
		DeepgramReconnectInitialDelay: "500ms",
		DeepgramReconnectMaxBackoff:   "30s",
		TTSMaxLength:                  5000,
	}
}

// Load reads configuration from a YAML file (if it exists), applies
// environment variable overrides, loads secrets, and validates the result.
// It returns the config, any validation warnings, and an error if the file
// exists but cannot be read or parsed.
func Load(path string) (Config, []string, error) {
	cfg := defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return cfg, nil, fmt.Errorf("read config file: %w", err)
			}
		} else {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, nil, fmt.Errorf("parse config file: %w", err)
			}
		}
	}

	applyEnvOverrides(&cfg)
	applyGenAIConfigDefaults(&cfg)
	loadSecrets(&cfg)

	warnings := validate(&cfg)
	return cfg, warnings, nil
}

// ParsedSilenceTimeout returns SilenceTimeout as a time.Duration,
// falling back to 30s if the value is invalid.
func (c *Config) ParsedSilenceTimeout() time.Duration {
	d, err := time.ParseDuration(c.SilenceTimeout)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

func (c *Config) ParsedDeepgramReconnectInitialDelay() time.Duration {
	d, err := time.ParseDuration(c.DeepgramReconnectInitialDelay)
	if err != nil {
		return 500 * time.Millisecond
	}
	return d
}

func (c *Config) ParsedDeepgramReconnectMaxBackoff() time.Duration {
	d, err := time.ParseDuration(c.DeepgramReconnectMaxBackoff)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// SampleRateCandidates returns a deduplicated ordered list of sample rates
// to try: preferred rate first, then configured alternatives, then defaults.
func (c *Config) SampleRateCandidates() []int {
	hardcoded := []int{16000, 48000, 44100, 32000, 24000}

	combined := make([]int, 0, 1+len(c.MicSampleRates)+len(hardcoded))
	combined = append(combined, c.MicSampleRate)
	combined = append(combined, c.MicSampleRates...)
	combined = append(combined, hardcoded...)

	seen := make(map[int]struct{}, len(combined))
	result := make([]int, 0, len(combined))
	for _, rate := range combined {
		if rate <= 0 {
			continue
		}
		if _, ok := seen[rate]; ok {
			continue
		}
		seen[rate] = struct{}{}
		result = append(result, rate)
	}
	return result
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv(EnvPrefix + "DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv(EnvPrefix + "AUDIO_DIR"); v != "" {
		cfg.AudioDir = v
	}
	if v := os.Getenv(EnvPrefix + "LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv(EnvPrefix + "SILENCE_TIMEOUT"); v != "" {
		cfg.SilenceTimeout = v
	}
	if v := os.Getenv(EnvPrefix + "MIC_SAMPLE_RATE"); v != "" {
		if rate, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && rate > 0 {
			cfg.MicSampleRate = rate
		}
	}
	if v := os.Getenv(EnvPrefix + "MIC_SAMPLE_RATES"); v != "" {
		cfg.MicSampleRates = parseSampleRates(v)
	}
	if v := os.Getenv(EnvPrefix + "SUMMARIZATION_MODEL"); v != "" {
		cfg.Summarization.Model = v
	}
	if v := os.Getenv(EnvPrefix + "GDRIVE_FOLDER_ID"); v != "" {
		cfg.GDriveFolderID = v
	}
	if v := os.Getenv(EnvPrefix + "GOOGLE_CREDENTIALS_FILE"); v != "" {
		cfg.GoogleCredentialsFile = v
	}
	if v := os.Getenv(EnvPrefix + "GCP_PROJECT"); v != "" {
		cfg.GCPProject = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "GCP_LOCATION"); v != "" {
		cfg.GCPLocation = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "GENAI_BACKEND"); v != "" {
		cfg.GenAIBackend = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv(EnvPrefix + "TRANSCRIPTION_ENDPOINTING"); v != "" {
		cfg.Transcription.Endpointing = v
	}
	if v := os.Getenv(EnvPrefix + "TRANSCRIPTION_UTTERANCE_END_MS"); v != "" {
		cfg.Transcription.UtteranceEndMs = v
	}
	if v := os.Getenv(EnvPrefix + "BATCH_TRANSCRIPTION_PROVIDER"); v != "" {
		cfg.BatchTranscription.Provider = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv(EnvPrefix + "BATCH_TRANSCRIPTION_MODEL"); v != "" {
		cfg.BatchTranscription.Model = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "EMBEDDING_MODEL"); v != "" {
		cfg.EmbeddingModel = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "DEEPGRAM_BUFFER_SIZE"); v != "" {
		if size, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && size > 0 {
			cfg.DeepgramBufferSize = size
		}
	}
	if v := os.Getenv(EnvPrefix + "DEEPGRAM_RECONNECT_INITIAL_DELAY"); v != "" {
		cfg.DeepgramReconnectInitialDelay = v
	}
	if v := os.Getenv(EnvPrefix + "DEEPGRAM_MODEL"); v != "" {
		cfg.DeepgramModel = v
	}
	if v := os.Getenv(EnvPrefix + "DEEPGRAM_RECONNECT_MAX_BACKOFF"); v != "" {
		cfg.DeepgramReconnectMaxBackoff = v
	}
	if v := os.Getenv(EnvPrefix + "GDRIVE_SYNC_ENABLED"); v != "" {
		cfg.GDriveSyncEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(EnvPrefix + "ENVOY_ENABLED"); v != "" {
		cfg.EnvoyEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(EnvPrefix + "NATS_URL"); v != "" {
		cfg.NATSURL = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "ENVOY_TOPIC"); v != "" {
		cfg.EnvoyTopic = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "GC_ENABLED"); v != "" {
		cfg.GCEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(EnvPrefix + "GC_MAX_AGE_DAYS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.GCMaxAgeDays = n
		}
	}
	if v := os.Getenv(EnvPrefix + "GC_MAX_AUDIO_SIZE_MB"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.GCMaxAudioSizeMB = n
		}
	}
	if v := os.Getenv(EnvPrefix + "SPEAKER_ENABLED"); v != "" {
		cfg.SpeakerEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(EnvPrefix + "SPEAKER_DEVICE"); v != "" {
		cfg.SpeakerDevice = v
	}
	if v := os.Getenv(EnvPrefix + "TTS_PROVIDER"); v != "" {
		cfg.TTSProvider = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv(EnvPrefix + "TTS_VOICE"); v != "" {
		cfg.TTSVoice = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "TTS_MAX_LENGTH"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.TTSMaxLength = n
		}
	}
}

func applyGenAIConfigDefaults(cfg *Config) {
	if cfg.GCPProject == "" {
		cfg.GCPProject = discoverServiceAccountProject(
			os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
			cfg.GoogleCredentialsFile,
		)
	}
	if strings.TrimSpace(cfg.GCPLocation) == "" {
		cfg.GCPLocation = genaiconfig.DefaultLocation
	}
	cfg.GenAIBackend = strings.ToLower(strings.TrimSpace(cfg.GenAIBackend))
	if cfg.GenAIBackend == "" {
		cfg.GenAIBackend = genaiconfig.DefaultBackend(cfg.GCPProject)
	}
}

func loadSecrets(cfg *Config) {
	cfg.DeepgramAPIKey = os.Getenv(EnvPrefix + "DEEPGRAM_API_KEY")
	cfg.OpenAIAPIKey = os.Getenv(EnvPrefix + "OPENAI_API_KEY")
	cfg.AnthropicAPIKey = os.Getenv(EnvPrefix + "ANTHROPIC_API_KEY")
	cfg.GeminiAPIKey = os.Getenv(EnvPrefix + "GEMINI_API_KEY")
	cfg.TTSAPIKey = os.Getenv(EnvPrefix + "TTS_API_KEY")
}

func discoverServiceAccountProject(paths ...string) string {
	type serviceAccount struct {
		ProjectID string `json:"project_id"`
	}

	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var creds serviceAccount
		if err := json.Unmarshal(data, &creds); err != nil {
			continue
		}
		if strings.TrimSpace(creds.ProjectID) != "" {
			return strings.TrimSpace(creds.ProjectID)
		}
	}

	return ""
}

func validate(cfg *Config) []string {
	var warnings []string

	if cfg.DeepgramAPIKey == "" {
		warnings = append(warnings, "Deepgram API key not configured — live transcription is disabled. Set "+EnvPrefix+"DEEPGRAM_API_KEY.")
	}

	if cfg.GenAIBackend != "" && genaiconfig.NormalizeBackend(cfg.GenAIBackend) == "" {
		warnings = append(warnings, fmt.Sprintf("Invalid genai_backend %q — using %s.", cfg.GenAIBackend, genaiconfig.DefaultBackend(cfg.GCPProject)))
		cfg.GenAIBackend = genaiconfig.DefaultBackend(cfg.GCPProject)
	}
	if strings.TrimSpace(cfg.GCPLocation) == "" {
		cfg.GCPLocation = genaiconfig.DefaultLocation
	}

	providers := make(map[string]struct{})
	addModelProvider := func(scope, model string) {
		provider, _, err := llm.ParseModel(model)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Invalid %s model %q — %v.", scope, model, err))
			return
		}
		providers[provider] = struct{}{}
	}

	addModelProvider("summarization", cfg.Summarization.Model)
	if strings.TrimSpace(cfg.EmbeddingModel) != "" {
		addModelProvider("embedding", cfg.EmbeddingModel)
	}

	if _, ok := cfg.Summarization.Presets["default"]; !ok {
		warnings = append(warnings, "No default summarization preset configured — set summarization.presets.default.")
	}

	for name, preset := range cfg.Summarization.Presets {
		if strings.TrimSpace(preset.Model) == "" {
			continue
		}
		addModelProvider(fmt.Sprintf("summarization preset %q", name), preset.Model)
	}

	for provider := range providers {
		switch provider {
		case "openai":
			if cfg.OpenAIAPIKey == "" {
				warnings = append(warnings, "OpenAI API key not configured — set "+EnvPrefix+"OPENAI_API_KEY.")
			}
		case "anthropic":
			if cfg.AnthropicAPIKey == "" {
				warnings = append(warnings, "Anthropic API key not configured — set "+EnvPrefix+"ANTHROPIC_API_KEY.")
			}
		case "gemini":
			if !genaiconfig.CanUse(&genaiconfig.Options{
				Backend:  cfg.GenAIBackend,
				Project:  cfg.GCPProject,
				Location: cfg.GCPLocation,
				APIKey:   cfg.GeminiAPIKey,
			}) {
				warnings = append(warnings, "Gemini backend not configured — set "+EnvPrefix+"GCP_PROJECT for Vertex AI or "+EnvPrefix+"GEMINI_API_KEY for Gemini API.")
			}
		}
	}

	if _, err := time.ParseDuration(cfg.SilenceTimeout); err != nil {
		warnings = append(warnings, fmt.Sprintf("Invalid silence_timeout %q — using default 30s.", cfg.SilenceTimeout))
	}

	if v := cfg.Transcription.Endpointing; v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			warnings = append(warnings, fmt.Sprintf("Invalid transcription.endpointing %q — must be a non-negative integer (ms). Using Deepgram default.", v))
		}
	}
	if v := cfg.Transcription.UtteranceEndMs; v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			warnings = append(warnings, fmt.Sprintf("Invalid transcription.utterance_end_ms %q — must be a non-negative integer (ms). Using Deepgram default.", v))
		}
	}

	if cfg.BatchTranscription.Provider == "" {
		cfg.BatchTranscription.Provider = "deepgram"
	}
	if cfg.EnvoyTopic == "" {
		cfg.EnvoyTopic = "notifications.ghost-wispr.summary-ready"
	}
	if cfg.EnvoyEnabled && strings.TrimSpace(cfg.NATSURL) == "" {
		warnings = append(warnings, "Envoy enabled but NATS URL not configured — set "+EnvPrefix+"NATS_URL or disable Envoy publishing.")
	}
	if cfg.BatchTranscription.Model == "" {
		cfg.BatchTranscription.Model = cfg.DeepgramModel
	}
	switch cfg.BatchTranscription.Provider {
	case "deepgram", "groq", "openai":
	default:
		warnings = append(warnings, fmt.Sprintf("Invalid batch_transcription.provider %q — using default deepgram.", cfg.BatchTranscription.Provider))
		cfg.BatchTranscription.Provider = "deepgram"
	}

	if cfg.GCMaxAgeDays <= 0 {
		warnings = append(warnings, "gc_max_age_days must be positive — using default 30.")
		cfg.GCMaxAgeDays = 30
	}
	if cfg.GCMaxAudioSizeMB <= 0 {
		warnings = append(warnings, "gc_max_audio_size_mb must be positive — using default 1024.")
		cfg.GCMaxAudioSizeMB = 1024
	}

	if cfg.TTSProvider != "" {
		switch cfg.TTSProvider {
		case "elevenlabs", "google":
		default:
			warnings = append(warnings, fmt.Sprintf("Invalid tts_provider %q — supported providers are elevenlabs, google.", cfg.TTSProvider))
		}
		if cfg.TTSAPIKey == "" && cfg.TTSProvider != "google" {
			warnings = append(warnings, "TTS provider configured but API key not set — set "+EnvPrefix+"TTS_API_KEY.")
		} else if cfg.TTSAPIKey == "" && cfg.TTSProvider == "google" && os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
			warnings = append(warnings, "Google TTS requires "+EnvPrefix+"TTS_API_KEY or GOOGLE_APPLICATION_CREDENTIALS.")
		}
		if cfg.TTSMaxLength <= 0 {
			cfg.TTSMaxLength = 5000
		}
	}

	return warnings
}

func parseSampleRates(raw string) []int {
	parts := strings.Split(raw, ",")
	seen := make(map[int]struct{}, len(parts))
	result := make([]int, 0, len(parts))

	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		rate, err := strconv.Atoi(trimmed)
		if err != nil || rate <= 0 {
			continue
		}
		if _, ok := seen[rate]; ok {
			continue
		}
		seen[rate] = struct{}{}
		result = append(result, rate)
	}

	return result
}
