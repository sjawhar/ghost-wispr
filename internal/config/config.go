package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvPrefix is the namespace prefix for all Ghost Wispr environment variables.
const EnvPrefix = "GHOST_WISPR_"

// Config holds all application configuration. Secrets (API keys) are loaded
// exclusively from environment variables and never appear in the config file.
type Config struct {
	DBPath                string `yaml:"db_path"`
	AudioDir              string `yaml:"audio_dir"`
	SilenceTimeout        string `yaml:"silence_timeout"`
	MicSampleRate         int    `yaml:"mic_sample_rate"`
	MicSampleRates        []int  `yaml:"mic_sample_rates"`
	OpenAIModel           string `yaml:"openai_model"`
	GDriveFolderID        string `yaml:"gdrive_folder_id"`
	GoogleCredentialsFile string `yaml:"google_credentials_file"`

	// Secrets — env vars only, never serialized to YAML.
	DeepgramAPIKey string `yaml:"-"`
	OpenAIAPIKey   string `yaml:"-"`
}

func defaults() Config {
	return Config{
		DBPath:                "data/ghost-wispr.db",
		AudioDir:              "data/audio",
		SilenceTimeout:        "30s",
		MicSampleRate:         16000,
		MicSampleRates:        []int{48000, 44100, 32000, 24000},
		OpenAIModel:           "gpt-4o-mini",
		GoogleCredentialsFile: "./service-account.json",
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
	if v := os.Getenv(EnvPrefix + "OPENAI_MODEL"); v != "" {
		cfg.OpenAIModel = v
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
}

func validate(cfg *Config) []string {
	var warnings []string

	if cfg.DeepgramAPIKey == "" {
		warnings = append(warnings, "Deepgram API key not configured \u2014 live transcription is disabled. Set "+EnvPrefix+"DEEPGRAM_API_KEY.")
	}
	if cfg.OpenAIAPIKey == "" {
		warnings = append(warnings, "OpenAI API key not configured \u2014 session summaries are disabled. Set "+EnvPrefix+"OPENAI_API_KEY.")
	}
	if _, err := time.ParseDuration(cfg.SilenceTimeout); err != nil {
		warnings = append(warnings, fmt.Sprintf("Invalid silence_timeout %q \u2014 using default 30s.", cfg.SilenceTimeout))
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
