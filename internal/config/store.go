package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Store holds mutable config with thread-safe access and YAML write-back.
type Store struct {
	mu       sync.RWMutex
	cfg      Config
	path     string
	envPath  string
	onChange []func(Config)
}

// NewStore loads config from the given YAML path (if it exists),
// applies env overrides, loads secrets, validates, and returns
// a mutable store.
func NewStore(path string) (*Store, []string, error) {
	cfg, warnings, err := Load(path)
	if err != nil {
		return nil, warnings, err
	}

	return &Store{cfg: cfg, path: path}, warnings, nil
}

// NewStoreWithEnv loads config from YAML, then secrets from .env file
// (environment variables still take precedence), and returns a mutable store.
func NewStoreWithEnv(yamlPath, envPath string) (*Store, []string, error) {
	envSecrets := loadEnvFile(envPath)

	cfg, warnings, err := Load(yamlPath)
	if err != nil {
		return nil, warnings, err
	}

	applyEnvFileSecrets(&cfg, envSecrets)

	return &Store{cfg: cfg, path: yamlPath, envPath: envPath}, warnings, nil
}

// Get returns a snapshot copy of the current config.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.copyConfig()
}

// Update applies a mutation to the config, validates the result,
// writes back to YAML if a path is configured, and fires onChange callbacks.
// If validation fails, the mutation is rolled back.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()

	prev := s.copyConfig()
	fn(&s.cfg)

	if err := s.validateForUpdate(); err != nil {
		s.cfg = prev
		s.mu.Unlock()
		return err
	}

	if s.path != "" {
		if err := s.writeYAML(); err != nil {
			s.cfg = prev
			s.mu.Unlock()
			return fmt.Errorf("write config: %w", err)
		}
	}

	if s.envPath != "" {
		if err := s.writeEnvFile(); err != nil {
			s.cfg = prev
			if s.path != "" {
				_ = s.writeYAML() // best-effort rollback of YAML to prev values
			}
			s.mu.Unlock()
			return fmt.Errorf("write .env: %w", err)
		}
	}

	current := s.copyConfig()
	callbacks := make([]func(Config), len(s.onChange))
	copy(callbacks, s.onChange)
	s.mu.Unlock()

	for _, cb := range callbacks {
		cb(current)
	}
	return nil
}

// OnChange registers a callback invoked after every successful Update.
func (s *Store) OnChange(fn func(Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = append(s.onChange, fn)
}

func (s *Store) copyConfig() Config {
	c := s.cfg

	if s.cfg.MicSampleRates != nil {
		c.MicSampleRates = make([]int, len(s.cfg.MicSampleRates))
		copy(c.MicSampleRates, s.cfg.MicSampleRates)
	}

	if s.cfg.Summarization.Presets != nil {
		c.Summarization.Presets = make(map[string]Preset, len(s.cfg.Summarization.Presets))
		for k, v := range s.cfg.Summarization.Presets {
			c.Summarization.Presets[k] = v
		}
	}

	return c
}

// validateForUpdate checks fields that the user can change via the API.
func (s *Store) validateForUpdate() error {
	if _, err := time.ParseDuration(s.cfg.SilenceTimeout); err != nil {
		return fmt.Errorf("invalid silence_timeout %q: %w", s.cfg.SilenceTimeout, err)
	}

	if s.cfg.Summarization.Model != "" {
		if err := validateModelFormat(s.cfg.Summarization.Model); err != nil {
			return err
		}
	}

	for name, preset := range s.cfg.Summarization.Presets {
		if preset.Model != "" {
			if err := validateModelFormat(preset.Model); err != nil {
				return fmt.Errorf("preset %q: %w", name, err)
			}
		}
	}

	if v := s.cfg.Transcription.Endpointing; v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			return fmt.Errorf("invalid transcription.endpointing %q: must be a non-negative integer", v)
		}
	}
	if v := s.cfg.Transcription.UtteranceEndMs; v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			return fmt.Errorf("invalid transcription.utterance_end_ms %q: must be a non-negative integer", v)
		}
	}

	return nil
}

func (s *Store) writeYAML() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(&s.cfg)
	if err != nil {
		return err
	}
	return atomicWriteFile(s.path, data, 0o644)
}

// validateModelFormat checks provider/model_name format without importing llm package.
func validateModelFormat(model string) error {
	for i, c := range model {
		if c == '/' {
			if i == 0 || i == len(model)-1 {
				return fmt.Errorf("invalid model format %q: expected provider/model_name", model)
			}
			return nil
		}
	}
	return fmt.Errorf("invalid model format %q: expected provider/model_name", model)
}

// secretEnvKeys maps Config secret fields to their environment variable names.
var secretEnvKeys = []struct {
	envKey string
	get    func(*Config) string
	set    func(*Config, string)
}{
	{EnvPrefix + "DEEPGRAM_API_KEY", func(c *Config) string { return c.DeepgramAPIKey }, func(c *Config, v string) { c.DeepgramAPIKey = v }},
	{EnvPrefix + "OPENAI_API_KEY", func(c *Config) string { return c.OpenAIAPIKey }, func(c *Config, v string) { c.OpenAIAPIKey = v }},
	{EnvPrefix + "ANTHROPIC_API_KEY", func(c *Config) string { return c.AnthropicAPIKey }, func(c *Config, v string) { c.AnthropicAPIKey = v }},
	{EnvPrefix + "GEMINI_API_KEY", func(c *Config) string { return c.GeminiAPIKey }, func(c *Config, v string) { c.GeminiAPIKey = v }},
}

// loadEnvFile reads KEY=VALUE pairs from a .env file.
func loadEnvFile(path string) map[string]string {
	result := make(map[string]string)
	if path == "" {
		return result
	}

	f, err := os.Open(path)
	if err != nil {
		return result
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		result[strings.TrimSpace(key)] = value
	}

	return result
}

// applyEnvFileSecrets sets secret fields from the .env map,
// but only if the corresponding environment variable is not already set.
func applyEnvFileSecrets(cfg *Config, envMap map[string]string) {
	for _, sk := range secretEnvKeys {
		// Environment variable already set by loadSecrets — don't override.
		if sk.get(cfg) != "" {
			continue
		}
		if v, ok := envMap[sk.envKey]; ok && v != "" {
			sk.set(cfg, v)
		}
	}
}

// writeEnvFile writes all non-empty secret values to the .env file.
func (s *Store) writeEnvFile() error {
	if err := os.MkdirAll(filepath.Dir(s.envPath), 0o755); err != nil {
		return err
	}

	// Read existing .env to preserve non-secret entries.
	existing := loadEnvFile(s.envPath)

	// Update secret entries.
	for _, sk := range secretEnvKeys {
		v := sk.get(&s.cfg)
		if v != "" {
			existing[sk.envKey] = v
		} else {
			delete(existing, sk.envKey)
		}
	}

	keys := make([]string, 0, len(existing))
	for k := range existing {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s\n", key, existing[key])
	}

	return atomicWriteFile(s.envPath, []byte(b.String()), 0o600)
}

// atomicWriteFile writes data to a temp file then renames it into place.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
