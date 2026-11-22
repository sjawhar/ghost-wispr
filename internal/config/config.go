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
	"unicode"

	"github.com/sjawhar/ghost-wispr/internal/logging"

	"gopkg.in/yaml.v3"
)

// EnvPrefix is the namespace prefix for all Ghost Wispr environment variables.
const EnvPrefix = "GHOST_WISPR_"

const (
	IndicatorLightBlink1 = "blink1"
	// leftoverEnvoyWebhookSecretWarning names a webhook signing key an earlier
	// provisioner left in the environment or .env.
	leftoverEnvoyWebhookSecretWarning = EnvPrefix + "ENVOY_WEBHOOK_SECRET is no longer read: Ghost Wispr publishes to the Envoy hub (envoy_hub_url), which needs no key. Remove it from the environment and .env."
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
	Model           string            `yaml:"model"` // provider/model
	Presets         map[string]Preset `yaml:"presets"`
	MinSummaryWords int               `yaml:"min_summary_words"`
	// BatchJobs summarizes a backlog of sessions as one batch job of
	// summarization.model's provider when it is the Gemini API (genai sends
	// Vertex AI no batch of inlined requests, so a Vertex AI provider
	// summarizes online whatever this says); false summarizes each online,
	// one call a session.
	BatchJobs bool `yaml:"batch_jobs"`
}

// Transcription is live transcription: the provider/model the microphone's
// stream goes to, and its turn settings.
type Transcription struct {
	Model          string `yaml:"model"` // provider/model
	Endpointing    string `yaml:"endpointing"`
	UtteranceEndMs string `yaml:"utterance_end_ms"`
}

// BatchTranscription is the provider/model a session's recording is
// transcribed again with after it ends.
type BatchTranscription struct {
	Model string `yaml:"model"` // provider/model; "" is transcription.model
}

// Embedding is the provider/model that indexes the transcripts for semantic
// search; "" turns embeddings off.
type Embedding struct {
	Model string `yaml:"model"`
}

// TTS is text-to-speech: the provider/model and voice that voice A and
// POST /api/tts/speak speak with.
type TTS struct {
	Model     string `yaml:"model"`      // provider/model; "" turns text-to-speech off
	Voice     string `yaml:"voice"`      // "" is the model's default voice
	MaxLength int    `yaml:"max_length"` // the longest text POST /api/tts/speak takes
}

// The voices, voice.engine's values.
const (
	VoiceEngineTTS     = "tts"     // voice A: text-to-speech, with the tts role's model
	VoiceEngineGPTLive = "gptlive" // voice B: speech-to-speech, OpenAI GPT-Live
	VoiceEngineDGAgent = "dgagent" // voice C: Deepgram's Voice Agent
)

// Voice configures Wispr's meeting voice: a person says a wake phrase on the
// live transcript, and a voice answers through the speaker.
type Voice struct {
	Enabled     bool     `yaml:"enabled"`
	Engine      string   `yaml:"engine"`       // tts, gptlive or dgagent
	WakePhrases []string `yaml:"wake_phrases"` // said at the start of a sentence
	// PausedListener makes a pause from the power button or the page go to
	// listening, where a listener in the process holds the microphone for
	// "Wispr, start recording" alone, in a build that has it (-tags kws) and
	// while the Jabra's mute button is attached; off, every pause closes the
	// microphone. It needs Enabled: with the voice off, nothing hears the room
	// for a wake phrase.
	PausedListener bool `yaml:"paused_listener"`
	// PausedListenerThreads is the spotter's threads, 1 but for the control
	// of the acceptance run's CPU check (GHOST_WISPR_VOICE_PAUSED_LISTENER_THREADS,
	// 1 to 8); never in the file.
	PausedListenerThreads int `yaml:"-"`
	// GPTLive is voice B's provider/model.
	GPTLive GPTLive `yaml:"gptlive"`
	// DGAgent is voice C's provider and the model that keeps its turns.
	DGAgent DGAgent `yaml:"dgagent"`
}

// GPTLive configures voice B (voice.engine gptlive).
type GPTLive struct {
	// Model is the provider/model of the Live session, on an OpenAI API
	// provider, such as openai/gpt-live-1.
	Model string `yaml:"model"`
}

// DGAgent configures voice C (voice.engine dgagent).
type DGAgent struct {
	// Provider names the Deepgram API provider whose key opens the agent.
	Provider string `yaml:"provider"`
	// ThinkModel is the model Deepgram runs to keep the voice's turns, by
	// Deepgram's own provider/model names, such as
	// google/gemini-3.1-flash-lite, since Deepgram's servers call it; "" is
	// the voice's default. The Concierge words every answer either way.
	ThinkModel string `yaml:"think_model"`
}

// ParseThinkModel reads a voice.dgagent.think_model name, provider/model,
// split at the first slash: the model may hold slashes. The config check
// and voice C both read the name with it. It refuses a name that is not
// provider/model, one with a space in it, and deepgram/think-fast, which
// Deepgram refuses with INVALID_SETTINGS in a session opted out of its
// model training, as every voice C session is.
func ParseThinkModel(name string) (provider, model string, err error) {
	name = strings.TrimSpace(name)
	provider, model, _ = strings.Cut(name, "/")
	if provider == "" || model == "" || strings.ContainsFunc(name, unicode.IsSpace) {
		return "", "", fmt.Errorf("think model %q is not provider/model, such as google/gemini-3.1-flash-lite", name)
	}
	if strings.EqualFold(provider, "deepgram") && strings.EqualFold(model, "think-fast") {
		return "", "", fmt.Errorf("think model %q needs Deepgram's model training, which voice C opts out of; use deepgram/phonellm-mini-alpha-2-fp8", name)
	}
	return provider, model, nil
}

// Concierge configures the agent loop that answers the voice from Dispatch
// and asks the agents that hold issues, through Envoy.
type Concierge struct {
	Model       string `yaml:"model"`
	DispatchURL string `yaml:"dispatch_url"`
	EnvoyURL    string `yaml:"envoy_url"`
}

type Config struct {
	DBPath                string   `yaml:"db_path"`
	AudioDir              string   `yaml:"audio_dir"`
	LogLevel              string   `yaml:"log_level"`
	PublicURL             string   `yaml:"public_url"`      // the unit's own HTTPS address, its tailnet name
	RecordingRules        []string `yaml:"recording_rules"` // the rules the web UI shows under Recording rules, as written; with none, it shows no button
	SilenceTimeout        string   `yaml:"silence_timeout"`
	MinSessionSegments    int      `yaml:"min_session_segments"`
	MicSampleRate         int      `yaml:"mic_sample_rate"`
	MicSampleRates        []int    `yaml:"mic_sample_rates"`
	MicDevice             string   `yaml:"mic_device"` // the sound card the microphone opens, by its ALSA id or name; empty opens the default input
	GDriveFolderID        string   `yaml:"gdrive_folder_id"`
	GoogleCredentialsFile string   `yaml:"google_credentials_file"`
	// Providers names each model provider once: its endpoint, wire API and
	// sign-in. Each role's provider/model names one of them. A file that
	// names none gets defaultProviders.
	Providers                     map[string]Provider `yaml:"providers"`
	GDriveSyncEnabled             bool                `yaml:"gdrive_sync_enabled"`
	EnvoyEnabled                  bool                `yaml:"envoy_enabled"`
	EnvoyHubURL                   string              `yaml:"envoy_hub_url"`
	GCEnabled                     bool                `yaml:"gc_enabled"`
	GCMaxAgeDays                  int                 `yaml:"gc_max_age_days"`
	GCMaxAudioSizeMB              int                 `yaml:"gc_max_audio_size_mb"`
	Summarization                 Summarization       `yaml:"summarization"`
	Transcription                 Transcription       `yaml:"transcription"`
	BatchTranscription            BatchTranscription  `yaml:"batch_transcription"`
	Embedding                     Embedding           `yaml:"embedding"`
	DeepgramBufferSize            int                 `yaml:"deepgram_buffer_size,omitempty"` // bytes of live audio held while Deepgram reconnects; 0 holds 60 s at the rate the microphone opened at, and is not written back on a save. The recorder keeps as much audio again, plus 35 s, in memory while no session records, so a recording can start before words Deepgram hears late
	DeepgramReconnectInitialDelay string              `yaml:"deepgram_reconnect_initial_delay"`
	DeepgramReconnectMaxBackoff   string              `yaml:"deepgram_reconnect_max_backoff"`
	SpeakerEnabled                bool                `yaml:"speaker_enabled"`
	SpeakerDevice                 string              `yaml:"speaker_device"`
	SpeakerTraceDir               string              `yaml:"speaker_trace_dir,omitempty"` // a debug setting, off when empty: the speaker keeps the last seconds of the process's execution trace and saves them here on each underrun (audio.SpeakerTrace)
	IndicatorLight                string              `yaml:"indicator_light"`
	PowerButton                   bool                `yaml:"power_button"`
	JabraMuteButton               bool                `yaml:"jabra_mute_button"`
	TTS                           TTS                 `yaml:"tts"`
	Voice                         Voice               `yaml:"voice"`
	Concierge                     Concierge           `yaml:"concierge"`

	// Secrets — env vars only, never serialized to YAML.
	// ProviderKeys are the keys of the api_key providers, by the variable
	// each one's auth.env names (APIKey reads one).
	ProviderKeys  map[string]string `yaml:"-"`
	DispatchToken string            `yaml:"-"` // the unit's own Dispatch credential, for the Concierge
	// keysSet are the variables of the keys SetAPIKey set: the only keys a
	// store writes to .env. A key only the environment holds, such as a
	// credential broker's short-lived one, stays off the disk.
	keysSet map[string]bool
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
		Providers:             defaultProviders(),
		GCMaxAgeDays:          30,
		GCMaxAudioSizeMB:      1024,
		Summarization: Summarization{
			Model:           "openai/gpt-4o-mini",
			MinSummaryWords: 20,
			BatchJobs:       true,
			Presets: map[string]Preset{
				"default": {
					Description:  "General-purpose summary adapted to content depth",
					SystemPrompt: "",
					UserTemplate: "{{transcript}}",
				},
			},
		},
		Transcription: Transcription{
			Model:          "deepgram/nova-3",
			Endpointing:    "400",
			UtteranceEndMs: "1000",
		},
		DeepgramBufferSize:            0,
		DeepgramReconnectInitialDelay: "500ms",
		DeepgramReconnectMaxBackoff:   "30s",
		TTS:                           TTS{MaxLength: 5000},
		Voice: Voice{
			Engine:                VoiceEngineTTS,
			WakePhrases:           []string{"Wispr", "Ghost Wispr"},
			GPTLive:               GPTLive{Model: "openai/gpt-live-1"},
			DGAgent:               DGAgent{Provider: "deepgram"},
			PausedListenerThreads: 1,
		},
	}
}

// oldDefaultDeepgramBufferSize is deepgram_buffer_size's default before it
// followed the microphone's rate: 60 s at 16000 Hz, which a settings save
// wrote into the file. Load reads it as 0, 60 s at the microphone's rate.
const oldDefaultDeepgramBufferSize = 1920000

// PathFromEnvironment is the config file the recorder and the commands beside
// it load: GHOST_WISPR_CONFIG, else ghost-wispr.yaml in the working
// directory.
func PathFromEnvironment() string {
	if path := os.Getenv(EnvPrefix + "CONFIG"); path != "" {
		return path
	}
	return "ghost-wispr.yaml"
}

// Load reads configuration from a YAML file (if it exists), applies
// environment variable overrides, loads secrets, and validates the result.
// It returns the config, any validation warnings, and an error if the file
// exists but cannot be read or parsed.
func Load(path string) (Config, []string, error) {
	cfg, _, warnings, err := load(path, nil)
	return cfg, warnings, err
}

// load is Load that also returns the log_level the config file names, or the
// default: GHOST_WISPR_LOG_LEVEL overrides it in memory only, so a settings
// change writes this one back. envFile is a .env file's entries, which give a
// provider its key when the environment does not.
func load(path string, envFile map[string]string) (cfg Config, fileLogLevel string, warnings []string, err error) {
	cfg = defaults()
	warnings = deprecatedEnvoyEnvironmentWarnings()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return cfg, "", nil, fmt.Errorf("read config file: %w", err)
			}
		} else {
			// A file that names its providers names its roles' models too:
			// the defaults that name the default providers do not apply, and
			// its providers replace the defaults rather than add to them, as
			// yaml.v3 would decode them into the default map.
			if namesProviders(data) {
				cfg.Providers = nil
				clearQuickstartModels(&cfg)
			}
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, "", nil, fmt.Errorf("parse config file: %w", err)
			}
			warnings = append(warnings, deprecatedEnvoyConfigWarnings(data)...)
			warnings = append(warnings, removedSettingWarnings(data)...)
		}
	}

	fileLogLevel = cfg.LogLevel
	warnings = append(warnings, applyEnvOverrides(&cfg)...)
	// A settings save under a build before the default followed the
	// microphone's rate wrote its default, 60 s at 16000 Hz, into the file;
	// read it as the default it was meant to be.
	if cfg.DeepgramBufferSize == oldDefaultDeepgramBufferSize {
		cfg.DeepgramBufferSize = 0
	}
	// An unknown light fails closed: dropping the setting would turn the light
	// check off and let the recorder capture with no light on.
	cfg.IndicatorLight = strings.ToLower(strings.TrimSpace(cfg.IndicatorLight))
	if cfg.IndicatorLight != "" && cfg.IndicatorLight != IndicatorLightBlink1 {
		return cfg, "", warnings, fmt.Errorf("indicator_light %q is not a known light: set it to %q, or leave it unset for a recorder with no light", cfg.IndicatorLight, IndicatorLightBlink1)
	}
	credentialPath := cfg.GoogleCredentialsFile
	if credentialPath == "" {
		credentialPath = os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	}
	applyProviderDefaults(&cfg, inspectGoogleCredentialFile(credentialPath).serviceAccountProject)
	loadSecrets(&cfg, envFile)

	warnings = append(warnings, removedEnvironmentWarnings(&cfg, envFile)...)
	warnings = append(warnings, validate(&cfg)...)
	return cfg, fileLogLevel, warnings, nil
}

// removedEnvoyKeys are the settings of the Envoy paths Ghost Wispr no longer
// has: NATS, then the webhook. A start that finds one says so, since the unit
// would otherwise send nothing and say nothing.
var removedEnvoyKeys = []string{"nats_url", "envoy_topic", "envoy_webhook_url", "envoy_webhook_secret_name"}

func deprecatedEnvoyConfigWarnings(data []byte) []string {
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil
	}

	var warnings []string
	for _, key := range removedEnvoyKeys {
		if _, found := values[key]; found {
			warnings = append(warnings, key+" has been removed; use envoy_hub_url instead.")
		}
	}
	return warnings
}

func deprecatedEnvoyEnvironmentWarnings() []string {
	var warnings []string
	for _, key := range removedEnvoyKeys {
		name := EnvPrefix + strings.ToUpper(key)
		if os.Getenv(name) != "" {
			warnings = append(warnings, name+" has been removed; use envoy_hub_url ("+EnvPrefix+"ENVOY_HUB_URL) instead.")
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
		cfg.Summarization.Model = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "GDRIVE_FOLDER_ID"); v != "" {
		cfg.GDriveFolderID = v
	}
	if v := os.Getenv(EnvPrefix + "GOOGLE_CREDENTIALS_FILE"); v != "" {
		cfg.GoogleCredentialsFile = v
	}
	if v := os.Getenv(EnvPrefix + "TRANSCRIPTION_ENDPOINTING"); v != "" {
		cfg.Transcription.Endpointing = v
	}
	if v := os.Getenv(EnvPrefix + "TRANSCRIPTION_UTTERANCE_END_MS"); v != "" {
		cfg.Transcription.UtteranceEndMs = v
	}
	if v := os.Getenv(EnvPrefix + "TRANSCRIPTION_MODEL"); v != "" {
		cfg.Transcription.Model = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "BATCH_TRANSCRIPTION_MODEL"); v != "" {
		cfg.BatchTranscription.Model = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "EMBEDDING_MODEL"); v != "" {
		cfg.Embedding.Model = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "DEEPGRAM_BUFFER_SIZE"); v != "" {
		if size, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && size > 0 {
			cfg.DeepgramBufferSize = size
		}
	}
	if v := os.Getenv(EnvPrefix + "DEEPGRAM_RECONNECT_INITIAL_DELAY"); v != "" {
		cfg.DeepgramReconnectInitialDelay = v
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
	if v := os.Getenv(EnvPrefix + "ENVOY_HUB_URL"); v != "" {
		cfg.EnvoyHubURL = strings.TrimSpace(v)
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
	if v := os.Getenv(EnvPrefix + "SPEAKER_TRACE_DIR"); v != "" {
		cfg.SpeakerTraceDir = strings.TrimSpace(v)
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
	if v := os.Getenv(EnvPrefix + "TTS_MODEL"); v != "" {
		cfg.TTS.Model = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "TTS_VOICE"); v != "" {
		cfg.TTS.Voice = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "TTS_MAX_LENGTH"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.TTS.MaxLength = n
		}
	}
	if v := os.Getenv(EnvPrefix + "VOICE_ENABLED"); v != "" {
		cfg.Voice.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	// An acceptance run turns the paused listener on, and its CPU check's
	// control runs the spotter on more threads (deploy/pi/accept-voice.sh).
	if v := os.Getenv(EnvPrefix + "VOICE_PAUSED_LISTENER"); v != "" {
		cfg.Voice.PausedListener = strings.EqualFold(v, "true") || v == "1"
	}
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvPrefix + "VOICE_PAUSED_LISTENER_THREADS"))); err == nil && n >= 1 && n <= 8 {
		cfg.Voice.PausedListenerThreads = n
	}
	for key, field := range map[string]*string{
		"VOICE_ENGINE":           &cfg.Voice.Engine,
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

// applyProviderDefaults gives a Vertex AI provider with no location the
// global one, and one with no project the project of a service-account
// credential file; an external-account file names no project to take.
func applyProviderDefaults(cfg *Config, serviceAccountProject string) {
	for name := range cfg.Providers {
		p := cfg.Providers[name]
		if p.API != APIVertex {
			continue
		}
		p.Project = strings.TrimSpace(p.Project)
		if p.Project == "" {
			p.Project = serviceAccountProject
		}
		p.Location = strings.TrimSpace(p.Location)
		if p.Location == "" {
			p.Location = DefaultVertexLocation
		}
		cfg.Providers[name] = p
	}
}

// loadSecrets reads each api_key provider's key from the variable its
// auth.env names, or from envFile when the environment has none, and the
// unit's Dispatch token.
func loadSecrets(cfg *Config, envFile map[string]string) {
	cfg.ProviderKeys = map[string]string{}
	for _, name := range cfg.keyEnvNames() {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			v = strings.TrimSpace(envFile[name])
		}
		if v != "" {
			cfg.ProviderKeys[name] = v
		}
	}
	cfg.DispatchToken = os.Getenv(EnvPrefix + "DISPATCH_TOKEN")
}

type googleCredentialInfo struct {
	serviceAccountProject string
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
	default:
		return googleCredentialInfo{}
	}
}

func validate(cfg *Config) []string {
	var warnings []string
	cfg.Voice.Engine = strings.ToLower(strings.TrimSpace(cfg.Voice.Engine))
	if strings.TrimSpace(cfg.BatchTranscription.Model) == "" {
		cfg.BatchTranscription.Model = cfg.Transcription.Model
	}
	if cfg.TTS.MaxLength <= 0 {
		cfg.TTS.MaxLength = 5000
	}

	if _, known := logging.LevelNamed(cfg.LogLevel); !known && strings.TrimSpace(cfg.LogLevel) != "" {
		where := "log_level"
		if os.Getenv(EnvPrefix+"LOG_LEVEL") != "" { // the override applyEnvOverrides took
			where = EnvPrefix + "LOG_LEVEL"
		}
		warnings = append(warnings, fmt.Sprintf("Invalid %s %q — logging at info. Set it to debug, info, warn or error.", where, cfg.LogLevel))
	}

	warnings = append(warnings, validateProviders(cfg.Providers)...)
	warnings = append(warnings, validateRoles(cfg)...)
	warnings = append(warnings, missingKeyWarnings(cfg)...)

	if _, ok := cfg.Summarization.Presets["default"]; !ok {
		warnings = append(warnings, "No default summarization preset configured — set summarization.presets.default.")
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

	cfg.EnvoyHubURL = strings.TrimSpace(cfg.EnvoyHubURL)
	if cfg.EnvoyEnabled {
		parsedURL, err := url.Parse(cfg.EnvoyHubURL)
		switch {
		case cfg.EnvoyHubURL == "":
			warnings = append(warnings, "Envoy enabled but its hub is not configured — set envoy_hub_url ("+EnvPrefix+"ENVOY_HUB_URL) to the hub listener's URL, or disable Envoy publishing.")
		case err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "":
			warnings = append(warnings, fmt.Sprintf("Invalid envoy_hub_url %q — it must be the absolute URL of the hub's listener, http or https, such as http://hub.example.ts.net:9020; Envoy events are not sent.", cfg.EnvoyHubURL))
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
	// A blank rule would show as an empty line under Recording rules.
	cfg.RecordingRules = slices.DeleteFunc(cfg.RecordingRules, func(rule string) bool { return strings.TrimSpace(rule) == "" })

	// 0 is each GC limit's "no limit"; a negative limit is a mistake.
	if cfg.GCMaxAgeDays < 0 {
		warnings = append(warnings, "gc_max_age_days must be 0 (no age limit) or positive — using default 30.")
		cfg.GCMaxAgeDays = 30
	}
	if cfg.GCMaxAudioSizeMB < 0 {
		warnings = append(warnings, "gc_max_audio_size_mb must be 0 (no audio budget) or positive — using default 1024.")
		cfg.GCMaxAudioSizeMB = 1024
	}

	warnings = append(warnings, validateVoice(cfg)...)
	return warnings
}

// validateVoice warns about a voice that cannot speak as configured.
func validateVoice(cfg *Config) []string {
	v := &cfg.Voice
	var warnings []string
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
	if v.Engine == VoiceEngineTTS && strings.TrimSpace(cfg.TTS.Model) == "" {
		warnings = append(warnings, "voice.engine tts needs tts.model — set it to the provider/model the voice speaks with.")
	}
	if strings.TrimSpace(cfg.Concierge.Model) == "" {
		warnings = append(warnings, "The voice needs concierge.model — set it to provider/model, such as vertex/gemini-3.5-flash-lite; the voice cannot answer.")
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
	v.DGAgent.ThinkModel = strings.TrimSpace(v.DGAgent.ThinkModel)
	if m := v.DGAgent.ThinkModel; m != "" {
		if _, _, err := ParseThinkModel(m); err != nil {
			warnings = append(warnings, fmt.Sprintf("voice.dgagent.think_model: %v — voice C cannot start.", err))
		}
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
