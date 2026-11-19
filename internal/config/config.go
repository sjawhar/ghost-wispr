package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/llm"

	"gopkg.in/yaml.v3"
)

// EnvPrefix is the namespace prefix for all Ghost Wispr environment variables.
const EnvPrefix = "GHOST_WISPR_"

const (
	IndicatorLightBlink1 = "blink1"
	// leftoverEnvoyWebhookSecretWarning names a signing key an earlier
	// provisioner left in the environment or .env.
	leftoverEnvoyWebhookSecretWarning = EnvPrefix + "ENVOY_WEBHOOK_SECRET is no longer read: Ghost Wispr reads the Envoy signing key from Secret Manager (envoy_webhook_secret_name). Remove it from the environment and .env."
)

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
	Endpointing    string `yaml:"endpointing"`
	UtteranceEndMs string `yaml:"utterance_end_ms"`
}

type BatchTranscription struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// The voices, voice.engine's values.
const (
	VoiceEngineTTS     = "tts"     // voice A: our own streaming text-to-speech
	VoiceEngineGPTLive = "gptlive" // voice B
	VoiceEngineDGAgent = "dgagent" // voice C
)

// Voice A's text-to-speech engines, voice.tts_engine's values.
const (
	TTSEngineGemini = "gemini" // Gemini TTS streamed from Vertex AI
	TTSEngineAura   = "aura"   // Deepgram Aura-2, with the Deepgram key
)

// Voice configures Wispr's meeting voice: a person says a wake phrase on the
// live transcript, and a voice answers through the speaker.
type Voice struct {
	Enabled     bool     `yaml:"enabled"`
	Engine      string   `yaml:"engine"`       // tts, gptlive or dgagent
	WakePhrases []string `yaml:"wake_phrases"` // said at the start of a sentence
	// PausedListener is the wake listener that holds the microphone while
	// paused (slice 4). It is not built yet; true is ignored, with a warning.
	PausedListener bool   `yaml:"paused_listener"`
	TTSEngine      string `yaml:"tts_engine"`   // voice A's engine: gemini or aura
	TTSModel       string `yaml:"tts_model"`    // "" is the engine's default
	TTSVoice       string `yaml:"tts_voice"`    // "" is the engine's default
	TTSLocation    string `yaml:"tts_location"` // the Vertex AI location Gemini streams from
	// GPTLive is voice B's OpenAI route: the workload identity ids its token
	// exchange names. They are not secrets.
	GPTLive GPTLive `yaml:"gptlive"`
}

// GPTLive configures voice B (voice.engine gptlive).
type GPTLive struct {
	OpenAIIdentityProviderID string `yaml:"openai_identity_provider_id"`
	OpenAIServiceAccountID   string `yaml:"openai_service_account_id"`
}

// Concierge configures the agent loop that answers the voice from Dispatch
// and asks the agents that hold issues, through Envoy.
type Concierge struct {
	Model       string `yaml:"model"`
	DispatchURL string `yaml:"dispatch_url"`
	EnvoyURL    string `yaml:"envoy_url"`
}

type Config struct {
	DBPath                        string             `yaml:"db_path"`
	AudioDir                      string             `yaml:"audio_dir"`
	LogLevel                      string             `yaml:"log_level"`
	PublicURL                     string             `yaml:"public_url"`      // the unit's own HTTPS address, its tailnet name
	RecordingRules                []string           `yaml:"recording_rules"` // the rules the web UI shows under Recording rules, as written; with none, it shows no button
	SilenceTimeout                string             `yaml:"silence_timeout"`
	MinSessionSegments            int                `yaml:"min_session_segments"`
	MicSampleRate                 int                `yaml:"mic_sample_rate"`
	MicSampleRates                []int              `yaml:"mic_sample_rates"`
	MicDevice                     string             `yaml:"mic_device"` // the sound card the microphone opens, by its ALSA id or name; empty opens the default input
	GDriveFolderID                string             `yaml:"gdrive_folder_id"`
	GoogleCredentialsFile         string             `yaml:"google_credentials_file"`
	GCPProject                    string             `yaml:"gcp_project"`
	GCPLocation                   string             `yaml:"gcp_location"`
	GenAIBackend                  string             `yaml:"genai_backend"`
	GDriveSyncEnabled             bool               `yaml:"gdrive_sync_enabled"`
	EnvoyEnabled                  bool               `yaml:"envoy_enabled"`
	EnvoyWebhookURL               string             `yaml:"envoy_webhook_url"`
	EnvoyWebhookSecretName        string             `yaml:"envoy_webhook_secret_name"`
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
	IndicatorLight                string             `yaml:"indicator_light"`
	PowerButton                   bool               `yaml:"power_button"`
	JabraMuteButton               bool               `yaml:"jabra_mute_button"`
	TTSProvider                   string             `yaml:"tts_provider"`
	TTSModel                      string             `yaml:"tts_model"`
	TTSVoice                      string             `yaml:"tts_voice"`
	TTSMaxLength                  int                `yaml:"tts_max_length"`
	Voice                         Voice              `yaml:"voice"`
	Concierge                     Concierge          `yaml:"concierge"`

	// Secrets — env vars only, never serialized to YAML.
	DeepgramAPIKey  string `yaml:"-"`
	OpenAIAPIKey    string `yaml:"-"`
	AnthropicAPIKey string `yaml:"-"`
	GeminiAPIKey    string `yaml:"-"`
	TTSAPIKey       string `yaml:"-"`
	DispatchToken   string `yaml:"-"` // the unit's own Dispatch credential, for the Concierge
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
		GoogleCredentialsFile: "",
		GCPLocation:           genaiconfig.DefaultLocation,
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
		Voice: Voice{
			Engine:      VoiceEngineTTS,
			WakePhrases: []string{"Wispr", "Ghost Wispr"},
			TTSEngine:   TTSEngineGemini,
			// Google: the us multi-region gives a lower time to first audio
			// than global.
			TTSLocation: "us",
		},
	}
}

// Load reads configuration from a YAML file (if it exists), applies
// environment variable overrides, loads secrets, and validates the result.
// It returns the config, any validation warnings, and an error if the file
// exists but cannot be read or parsed.
func Load(path string) (Config, []string, error) {
	cfg := defaults()
	warnings := deprecatedEnvoyEnvironmentWarnings()

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
			warnings = append(warnings, deprecatedEnvoyConfigWarnings(data)...)
		}
	}

	warnings = append(warnings, applyEnvOverrides(&cfg)...)
	// An unknown light fails closed: dropping the setting would turn the light
	// check off and let the recorder capture with no light on.
	cfg.IndicatorLight = strings.ToLower(strings.TrimSpace(cfg.IndicatorLight))
	if cfg.IndicatorLight != "" && cfg.IndicatorLight != IndicatorLightBlink1 {
		return cfg, warnings, fmt.Errorf("indicator_light %q is not a known light: set it to %q, or leave it unset for a recorder with no light", cfg.IndicatorLight, IndicatorLightBlink1)
	}
	credentialPath := cfg.GoogleCredentialsFile
	if credentialPath == "" {
		credentialPath = os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	}
	googleCredentialInfo := inspectGoogleCredentialFile(credentialPath)
	applyGenAIConfigDefaults(&cfg, googleCredentialInfo.serviceAccountProject)
	loadSecrets(&cfg)

	warnings = append(warnings, validate(&cfg, googleCredentialInfo.hasExternalAccount)...)
	return cfg, warnings, nil
}

func deprecatedEnvoyConfigWarnings(data []byte) []string {
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil
	}

	var warnings []string
	for _, key := range []string{"nats_url", "envoy_topic"} {
		if _, found := values[key]; found {
			warnings = append(warnings, key+" has been removed; use envoy_webhook_url instead.")
		}
	}
	return warnings
}

func deprecatedEnvoyEnvironmentWarnings() []string {
	var warnings []string
	for _, key := range []string{"NATS_URL", "ENVOY_TOPIC"} {
		if _, found := os.LookupEnv(EnvPrefix + key); found {
			warnings = append(warnings, EnvPrefix+key+" has been removed; use "+EnvPrefix+"ENVOY_WEBHOOK_URL instead.")
		}
	}
	if os.Getenv(EnvPrefix+"ENVOY_WEBHOOK_SECRET") != "" {
		warnings = append(warnings, leftoverEnvoyWebhookSecretWarning)
	}
	return warnings
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

// applyEnvOverrides sets each setting its environment variable names, and
// returns a warning for an override it cannot read.
func applyEnvOverrides(cfg *Config) []string {
	var warnings []string
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
	if v := os.Getenv(EnvPrefix + "ENVOY_WEBHOOK_URL"); v != "" {
		cfg.EnvoyWebhookURL = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "ENVOY_WEBHOOK_SECRET_NAME"); v != "" {
		cfg.EnvoyWebhookSecretName = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "PUBLIC_URL"); v != "" {
		cfg.PublicURL = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "RECORDING_RULES"); v != "" {
		var rules []string
		if err := json.Unmarshal([]byte(v), &rules); err != nil {
			warnings = append(warnings, "Invalid "+EnvPrefix+`RECORDING_RULES — it must be a JSON list of strings, such as ["First rule.", "Second rule."]; using recording_rules from the config file.`)
		} else {
			cfg.RecordingRules = rules
		}
	}
	if v := os.Getenv(EnvPrefix + "GC_ENABLED"); v != "" {
		cfg.GCEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(EnvPrefix + "GC_MAX_AGE_DAYS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.GCMaxAgeDays = n
		}
	}
	if v := os.Getenv(EnvPrefix + "GC_MAX_AUDIO_SIZE_MB"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.GCMaxAudioSizeMB = n
		}
	}
	if v := os.Getenv(EnvPrefix + "SPEAKER_ENABLED"); v != "" {
		cfg.SpeakerEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(EnvPrefix + "SPEAKER_DEVICE"); v != "" {
		cfg.SpeakerDevice = v
	}
	if v := os.Getenv(EnvPrefix + "MIC_DEVICE"); v != "" {
		cfg.MicDevice = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "INDICATOR_LIGHT"); v != "" {
		cfg.IndicatorLight = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv(EnvPrefix + "POWER_BUTTON"); v != "" {
		cfg.PowerButton = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(EnvPrefix + "JABRA_MUTE_BUTTON"); v != "" {
		cfg.JabraMuteButton = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(EnvPrefix + "TTS_PROVIDER"); v != "" {
		cfg.TTSProvider = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv(EnvPrefix + "TTS_MODEL"); v != "" {
		cfg.TTSModel = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "TTS_VOICE"); v != "" {
		cfg.TTSVoice = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "TTS_MAX_LENGTH"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.TTSMaxLength = n
		}
	}
	if v := os.Getenv(EnvPrefix + "VOICE_ENABLED"); v != "" {
		cfg.Voice.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	for key, field := range map[string]*string{
		"VOICE_ENGINE":           &cfg.Voice.Engine,
		"VOICE_TTS_ENGINE":       &cfg.Voice.TTSEngine,
		"VOICE_TTS_MODEL":        &cfg.Voice.TTSModel,
		"VOICE_TTS_VOICE":        &cfg.Voice.TTSVoice,
		"VOICE_TTS_LOCATION":     &cfg.Voice.TTSLocation,
		"CONCIERGE_MODEL":        &cfg.Concierge.Model,
		"CONCIERGE_DISPATCH_URL": &cfg.Concierge.DispatchURL,
		"CONCIERGE_ENVOY_URL":    &cfg.Concierge.EnvoyURL,
	} {
		if v := strings.TrimSpace(os.Getenv(EnvPrefix + key)); v != "" {
			*field = v
		}
	}
	return warnings
}

func applyGenAIConfigDefaults(cfg *Config, serviceAccountProject string) {
	if cfg.GCPProject == "" {
		cfg.GCPProject = serviceAccountProject
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
	cfg.DispatchToken = os.Getenv(EnvPrefix + "DISPATCH_TOKEN")
}

type googleCredentialInfo struct {
	serviceAccountProject string
	hasExternalAccount    bool
}

func inspectGoogleCredentialFile(path string) googleCredentialInfo {
	type credentialFile struct {
		Type      string `json:"type"`
		ProjectID string `json:"project_id"`
	}

	path = strings.TrimSpace(path)
	if path == "" {
		return googleCredentialInfo{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return googleCredentialInfo{}
	}
	var credential credentialFile
	if err := json.Unmarshal(data, &credential); err != nil {
		return googleCredentialInfo{}
	}
	switch credential.Type {
	case "service_account":
		return googleCredentialInfo{serviceAccountProject: strings.TrimSpace(credential.ProjectID)}
	case "external_account":
		return googleCredentialInfo{hasExternalAccount: true}
	default:
		return googleCredentialInfo{}
	}
}

func validate(cfg *Config, hasExternalAccount bool) []string {
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

	if cfg.GCPProject == "" && hasExternalAccount {
		warnings = append(warnings, "Set gcp_project: an external-account credential file names no project.")
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
	cfg.EnvoyWebhookSecretName = strings.TrimSpace(cfg.EnvoyWebhookSecretName)
	if cfg.EnvoyEnabled {
		webhookURL := strings.TrimSpace(cfg.EnvoyWebhookURL)
		switch webhookURL {
		case "":
			warnings = append(warnings, "Envoy enabled but webhook URL not configured — set "+EnvPrefix+"ENVOY_WEBHOOK_URL or disable Envoy publishing.")
		default:
			parsedURL, err := url.Parse(webhookURL)
			if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
				warnings = append(warnings, "Envoy webhook URL must be an absolute HTTPS URL.")
			}
		}
		// No signing key secret is a default: each deployment names its own.
		if cfg.EnvoyWebhookSecretName == "" {
			warnings = append(warnings, "Envoy enabled but its signing key secret is not named — set envoy_webhook_secret_name ("+EnvPrefix+"ENVOY_WEBHOOK_SECRET_NAME) to its Secret Manager version, or disable Envoy publishing.")
		}
	}
	if publicURL := strings.TrimSpace(cfg.PublicURL); publicURL != "" {
		parsed, err := url.Parse(publicURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			warnings = append(warnings, fmt.Sprintf("Invalid public_url %q — it must be the unit's HTTPS address with no path, such as https://ghost-wispr.example.ts.net; ignoring it.", cfg.PublicURL))
			cfg.PublicURL = ""
		} else {
			cfg.PublicURL = parsed.Scheme + "://" + parsed.Host
		}
	}
	if cfg.BatchTranscription.Model == "" {
		cfg.BatchTranscription.Model = cfg.DeepgramModel
	}
	// A blank rule would show as an empty line under Recording rules.
	cfg.RecordingRules = slices.DeleteFunc(cfg.RecordingRules, func(rule string) bool { return strings.TrimSpace(rule) == "" })
	switch cfg.BatchTranscription.Provider {
	case "deepgram", "groq", "openai":
	default:
		warnings = append(warnings, fmt.Sprintf("Invalid batch_transcription.provider %q — using default deepgram.", cfg.BatchTranscription.Provider))
		cfg.BatchTranscription.Provider = "deepgram"
	}

	// 0 is each GC limit's "no limit"; a negative limit is a mistake.
	if cfg.GCMaxAgeDays < 0 {
		warnings = append(warnings, "gc_max_age_days must be 0 (no age limit) or positive — using default 30.")
		cfg.GCMaxAgeDays = 30
	}
	if cfg.GCMaxAudioSizeMB < 0 {
		warnings = append(warnings, "gc_max_audio_size_mb must be 0 (no audio budget) or positive — using default 1024.")
		cfg.GCMaxAudioSizeMB = 1024
	}

	if cfg.TTSProvider != "" {
		switch cfg.TTSProvider {
		case "elevenlabs", "google", "gemini":
		default:
			warnings = append(warnings, fmt.Sprintf("Invalid tts_provider %q — supported providers are elevenlabs, gemini, google.", cfg.TTSProvider))
		}
		if cfg.TTSProvider == "gemini" && strings.TrimSpace(cfg.GCPProject) == "" {
			warnings = append(warnings, "Gemini TTS provider requires gcp_project for Vertex AI.")
		}
		if cfg.TTSAPIKey == "" && cfg.TTSProvider != "google" && cfg.TTSProvider != "gemini" {
			warnings = append(warnings, "TTS provider configured but API key not set — set "+EnvPrefix+"TTS_API_KEY.")
		}
		if cfg.TTSMaxLength <= 0 {
			cfg.TTSMaxLength = 5000
		}
	}
	warnings = append(warnings, validateVoice(cfg)...)
	return warnings
}

// validateVoice warns about a voice that cannot speak as configured.
func validateVoice(cfg *Config) []string {
	v := &cfg.Voice
	v.Engine = strings.ToLower(strings.TrimSpace(v.Engine))
	v.TTSEngine = strings.ToLower(strings.TrimSpace(v.TTSEngine))
	var warnings []string
	if v.PausedListener {
		warnings = append(warnings, "voice.paused_listener is not built yet; it is ignored, and a pause closes the microphone.")
	}
	if !v.Enabled {
		return warnings
	}
	if !cfg.SpeakerEnabled {
		warnings = append(warnings, "voice.enabled needs speaker_enabled: true — the voice is off.")
	}
	switch v.Engine {
	case VoiceEngineTTS, VoiceEngineGPTLive, VoiceEngineDGAgent:
	default:
		warnings = append(warnings, fmt.Sprintf("Invalid voice.engine %q — supported engines are tts, gptlive, dgagent.", v.Engine))
	}
	switch v.TTSEngine {
	case TTSEngineGemini:
		if v.Engine == VoiceEngineTTS && strings.TrimSpace(cfg.GCPProject) == "" {
			warnings = append(warnings, "voice.tts_engine gemini needs gcp_project for Vertex AI.")
		}
	case TTSEngineAura:
		if v.Engine == VoiceEngineTTS && cfg.DeepgramAPIKey == "" {
			warnings = append(warnings, "voice.tts_engine aura needs the Deepgram key — set "+EnvPrefix+"DEEPGRAM_API_KEY.")
		}
	default:
		warnings = append(warnings, fmt.Sprintf("Invalid voice.tts_engine %q — supported engines are gemini, aura.", v.TTSEngine))
	}
	phrases := v.WakePhrases[:0]
	for _, p := range v.WakePhrases {
		if p = strings.TrimSpace(p); p != "" {
			phrases = append(phrases, p)
		}
	}
	v.WakePhrases = phrases
	if len(v.WakePhrases) == 0 {
		warnings = append(warnings, "voice.wake_phrases is empty — nothing can call the voice.")
	}
	for name, raw := range map[string]string{"concierge.dispatch_url": cfg.Concierge.DispatchURL, "concierge.envoy_url": cfg.Concierge.EnvoyURL} {
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			warnings = append(warnings, name+" must be an absolute http or https URL — the Concierge cannot reach it.")
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
