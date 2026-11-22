package config

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Store holds mutable config with thread-safe access and YAML write-back.
type Store struct {
	mu  sync.RWMutex
	cfg Config
	// writtenLogLevel is the log_level a settings change writes back: the
	// config file's, or the one a change set. GHOST_WISPR_LOG_LEVEL's override
	// stays in memory, so a level set for one run does not stay in the file. A
	// change counts as setting the level when it changes the running one, so a
	// change to the level already running writes nothing new; no settings
	// field sets the level today.
	writtenLogLevel string
	path            string
	envPath         string
	onChange        []func(Config)
}

// NewStore loads config from the given YAML path (if it exists),
// applies env overrides, loads secrets, validates, and returns
// a mutable store.
func NewStore(path string) (*Store, []string, error) {
	cfg, fileLogLevel, warnings, err := load(path, nil)
	if err != nil {
		return nil, warnings, err
	}

	return &Store{cfg: cfg, writtenLogLevel: fileLogLevel, path: path}, warnings, nil
}

// NewStoreWithEnv loads config from YAML, then provider keys from the .env
// file where the environment sets none, and returns a mutable store.
func NewStoreWithEnv(yamlPath, envPath string) (*Store, []string, error) {
	envSecrets := loadEnvFile(envPath)

	cfg, fileLogLevel, warnings, err := load(yamlPath, envSecrets)
	if err != nil {
		return nil, warnings, err
	}

	// Load warned about a leftover signing key in the environment; one left
	// only in .env is warned about here.
	if envSecrets[EnvPrefix+"ENVOY_WEBHOOK_SECRET"] != "" && os.Getenv(EnvPrefix+"ENVOY_WEBHOOK_SECRET") == "" {
		warnings = append(warnings, leftoverEnvoyWebhookSecretWarning)
	}

	return &Store{cfg: cfg, writtenLogLevel: fileLogLevel, path: yamlPath, envPath: envPath}, warnings, nil
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
	return s.TryUpdate(func(c *Config) error {
		fn(c)
		return nil
	})
}

// TryUpdate is Update with a mutation that can refuse: when fn returns an
// error, the mutation is rolled back, nothing is written and no callback
// fires, and TryUpdate returns that error.
func (s *Store) TryUpdate(fn func(*Config) error) error {
	s.mu.Lock()

	prev := s.copyConfig()
	if err := fn(&s.cfg); err != nil {
		s.cfg = prev
		s.mu.Unlock()
		return err
	}

	if err := s.validateForUpdate(); err != nil {
		s.cfg = prev
		s.mu.Unlock()
		return err
	}

	writtenLogLevel := s.writtenLogLevel
	if s.cfg.LogLevel != prev.LogLevel {
		writtenLogLevel = s.cfg.LogLevel
	}
	if s.path != "" {
		if err := s.writeYAML(writtenLogLevel); err != nil {
			s.cfg = prev
			s.mu.Unlock()
			return fmt.Errorf("write config: %w", err)
		}
	}

	if s.envPath != "" {
		if err := s.writeEnvFile(); err != nil {
			s.cfg = prev
			if s.path != "" {
				_ = s.writeYAML(s.writtenLogLevel) // best-effort rollback of YAML to prev values
			}
			s.mu.Unlock()
			return fmt.Errorf("write .env: %w", err)
		}
	}
	s.writtenLogLevel = writtenLogLevel

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
	c.RecordingRules = slices.Clone(s.cfg.RecordingRules)
	c.Summarization.Presets = maps.Clone(s.cfg.Summarization.Presets)
	c.Providers = maps.Clone(s.cfg.Providers)
	c.ProviderKeys = maps.Clone(s.cfg.ProviderKeys)
	c.keysSet = maps.Clone(s.cfg.keysSet)

	return c
}

// validateForUpdate checks fields that the user can change via the API.
func (s *Store) validateForUpdate() error {
	if _, err := time.ParseDuration(s.cfg.SilenceTimeout); err != nil {
		return fmt.Errorf("invalid silence_timeout %q: %w", s.cfg.SilenceTimeout, err)
	}

	if err := validateRoleChange(&s.cfg); err != nil {
		return err
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

// writeYAML writes the config to the file, with logLevel as its log_level.
func (s *Store) writeYAML(logLevel string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	written := s.cfg
	written.LogLevel = logLevel
	data, err := yaml.Marshal(&written)
	if err != nil {
		return err
	}
	return atomicWriteFile(s.path, data, 0o644)
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

// writeEnvFile writes the provider keys the settings set (SetAPIKey) to the
// .env file, keeping its other lines. A key from the environment alone, or
// the one .env holds, is left as it is.
func (s *Store) writeEnvFile() error {
	if err := os.MkdirAll(filepath.Dir(s.envPath), 0o755); err != nil {
		return err
	}

	// Read existing .env to preserve non-secret entries.
	existing := loadEnvFile(s.envPath)

	// Update the keys set, each under the variable its auth.env names.
	for _, name := range s.cfg.keyEnvNames() {
		if !s.cfg.keysSet[name] {
			continue
		}
		if v := s.cfg.ProviderKeys[name]; v != "" {
			existing[name] = v
		} else {
			delete(existing, name)
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
