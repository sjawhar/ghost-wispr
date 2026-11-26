package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
)

// Revision names a state of the config: Count rises by one with each change
// in this run of the process, which Run names, so a client can tell which of
// two answers holds the later state, whatever order they reach it in. Run
// changes with each start, and Count starts again from 0.
type Revision struct {
	Run   string `json:"run"`
	Count uint64 `json:"count"`
}

// newRun returns a random name for this run of the process.
func newRun() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail
	return hex.EncodeToString(b)
}

// Store holds mutable config with thread-safe access. A change writes the
// settings it changed into the local settings file.
type Store struct {
	mu  sync.RWMutex
	cfg Config
	// releaseSettings is the release config alone, as the YAML map a config
	// file holds, read when the store loaded, with no local settings and no
	// environment: a change that sets a setting back to its value here
	// removes that setting from the local settings file.
	releaseSettings map[string]any
	// localSettings are the settings the running config holds from the local
	// settings file: those the start applied, as each change since has
	// changed them, in the form that file writes them. Nil when it holds
	// none.
	localSettings map[string]any
	// localSettingsErr is why the start did not apply the local settings
	// file, or nil.
	localSettingsErr error
	// localSettingsRefusal refuses each change while the local settings file
	// that the start could not apply is in place: a change would replace that
	// file, and the settings saved in it would be lost. Nil when the start
	// applied the file or found none.
	localSettingsRefusal error
	path                 string
	envPath              string
	onChange             []func(Config)
	revision             Revision
}

// NewStore loads config from the given release YAML path and its local
// settings file (if they exist), applies env overrides, loads secrets,
// validates, and returns a mutable store.
func NewStore(path string) (*Store, []string, error) {
	return newStore(path, "", nil)
}

// NewStoreWithEnv loads config from the release YAML and its local settings,
// then provider keys from the .env file where the environment sets none, and
// returns a mutable store.
func NewStoreWithEnv(yamlPath, envPath string) (*Store, []string, error) {
	envSecrets := loadEnvFile(envPath)
	store, warnings, err := newStore(yamlPath, envPath, envSecrets)
	if err != nil {
		return nil, warnings, err
	}

	// Load warned about a leftover signing key in the environment; one left
	// only in .env is warned about here.
	if envSecrets[EnvPrefix+"ENVOY_WEBHOOK_SECRET"] != "" && os.Getenv(EnvPrefix+"ENVOY_WEBHOOK_SECRET") == "" {
		warnings = append(warnings, leftoverEnvoyWebhookSecretWarning)
	}
	return store, warnings, nil
}

func newStore(path, envPath string, envSecrets map[string]string) (*Store, []string, error) {
	l, err := loadConfig(path, envSecrets)
	if err != nil {
		return nil, l.warnings, err
	}
	store := &Store{
		cfg:              l.cfg,
		releaseSettings:  l.releaseSettings,
		localSettings:    l.localSettings,
		localSettingsErr: l.localSettingsErr,
		path:             path,
		envPath:          envPath,
		revision:         Revision{Run: newRun()},
	}
	if l.localSettingsErr != nil {
		store.localSettingsRefusal = fmt.Errorf("settings not saved: Ghost Wispr did not apply %s at start (%v). Correct or remove that file, then restart Ghost Wispr. A save now would replace the settings in it", LocalSettingsPath(path), l.localSettingsErr)
	}
	return store, l.warnings, nil
}

// Get returns a snapshot copy of the current config.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.copyConfig()
}

// GetRevision returns a snapshot copy of the current config and the revision
// it is.
func (s *Store) GetRevision() (Config, Revision) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.copyConfig(), s.revision
}

// GetWithLocalSettings returns, from one state of the store, a snapshot copy
// of the current config, the revision it is, and the local settings it holds,
// as copies.
func (s *Store) GetWithLocalSettings() (Config, Revision, AppliedLocalSettings) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.copyConfig(), s.revision, AppliedLocalSettings{Settings: reportedSettings(s.localSettings), Err: s.localSettingsErr}
}

// Update applies a mutation to the config, validates the result, writes the
// local settings file if a path is configured, and fires onChange callbacks.
// If validation fails, the mutation is rolled back.
func (s *Store) Update(fn func(*Config)) error {
	return s.TryUpdate(func(c *Config) error {
		fn(c)
		return nil
	})
}

// TryUpdate is Update with a mutation that can refuse: when fn returns an
// error, the mutation is rolled back, nothing is written and no callback
// fires, and TryUpdate returns that error. While the local settings file
// that the start could not apply is in place, TryUpdate refuses each change
// before fn runs.
func (s *Store) TryUpdate(fn func(*Config) error) error {
	s.mu.Lock()
	if s.localSettingsRefusal != nil {
		s.mu.Unlock()
		return s.localSettingsRefusal
	}

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

	providerKeysChanged := !maps.Equal(s.cfg.ProviderKeys, prev.ProviderKeys) ||
		!maps.Equal(s.cfg.keysSet, prev.keysSet)
	var previousLocal localSettingsFile
	var localChanges map[string]any
	if s.path != "" {
		var err error
		if previousLocal, err = readLocalSettingsFile(s.path); err != nil {
			s.cfg = prev
			s.mu.Unlock()
			return fmt.Errorf("read local settings: %w", err)
		}
		if localChanges, err = s.writeLocalSettings(&prev, previousLocal); err != nil {
			s.cfg = prev
			s.mu.Unlock()
			return fmt.Errorf("write local settings: %w", err)
		}
	}

	if s.envPath != "" && providerKeysChanged {
		if err := s.writeEnvFile(); err != nil {
			s.cfg = prev
			err = fmt.Errorf("write .env: %w", err)
			if s.path != "" {
				if restoreErr := previousLocal.restore(); restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("put back the local settings file: %w", restoreErr))
				}
			}
			s.mu.Unlock()
			return err
		}
	}
	s.applyLocalChanges(localChanges)
	s.revision.Count++

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
