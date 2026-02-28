package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/llm"

	"gopkg.in/yaml.v3"
)

// EnvPrefix is the namespace prefix for all Ghost Wispr environment variables.
const EnvPrefix = "GHOST_WISPR_"

// Config holds all application configuration. Secrets (API keys) are loaded
// exclusively from environment variables and never appear in the config file.
type Preset struct {
	Description  string `yaml:"description"`
	SystemPrompt string `yaml:"system_prompt"`
	UserTemplate string `yaml:"user_template"`
	Model        string `yaml:"model"`
}

type Summarization struct {
	Model   string            `yaml:"model"`
	BaseURL string            `yaml:"base_url"`
	Presets map[string]Preset `yaml:"presets"`
}

type Config struct {
	DBPath                string        `yaml:"db_path"`
	AudioDir              string        `yaml:"audio_dir"`
	SilenceTimeout        string        `yaml:"silence_timeout"`
	MicSampleRate         int           `yaml:"mic_sample_rate"`
	MicSampleRates        []int         `yaml:"mic_sample_rates"`
	GDriveFolderID        string        `yaml:"gdrive_folder_id"`
	GoogleCredentialsFile string        `yaml:"google_credentials_file"`
	Summarization         Summarization `yaml:"summarization"`

	// Secrets — env vars only, never serialized to YAML.
	DeepgramAPIKey  string `yaml:"-"`
	OpenAIAPIKey    string `yaml:"-"`
	AnthropicAPIKey string `yaml:"-"`
	GeminiAPIKey    string `yaml:"-"`
}

func defaults() Config {
	return Config{
		DBPath:                "data/ghost-wispr.db",
		AudioDir:              "data/audio",
		SilenceTimeout:        "30s",
		MicSampleRate:         16000,
		MicSampleRates:        []int{48000, 44100, 32000, 24000},
		GoogleCredentialsFile: "./service-account.json",
		Summarization: Summarization{
			Model: "openai/gpt-4o-mini",
			Presets: map[string]Preset{
				"default": {
					Description:  "General-purpose meeting summary with key topics, decisions, and action items",
					SystemPrompt: "Summarize the following office conversation transcript concisely in markdown. Include key topics, decisions made, and action items if any.",
					UserTemplate: "{{transcript}}",
				},
			},
		},
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
}

func loadSecrets(cfg *Config) {
	cfg.DeepgramAPIKey = os.Getenv(EnvPrefix + "DEEPGRAM_API_KEY")
	cfg.OpenAIAPIKey = os.Getenv(EnvPrefix + "OPENAI_API_KEY")
	cfg.AnthropicAPIKey = os.Getenv(EnvPrefix + "ANTHROPIC_API_KEY")
	cfg.GeminiAPIKey = os.Getenv(EnvPrefix + "GEMINI_API_KEY")
}

func validate(cfg *Config) []string {
	var warnings []string

	if cfg.DeepgramAPIKey == "" {
		warnings = append(warnings, "Deepgram API key not configured — live transcription is disabled. Set "+EnvPrefix+"DEEPGRAM_API_KEY.")
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
			if cfg.GeminiAPIKey == "" {
				warnings = append(warnings, "Gemini API key not configured — set "+EnvPrefix+"GEMINI_API_KEY.")
			}
		}
	}

	if _, err := time.ParseDuration(cfg.SilenceTimeout); err != nil {
		warnings = append(warnings, fmt.Sprintf("Invalid silence_timeout %q — using default 30s.", cfg.SilenceTimeout))
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
