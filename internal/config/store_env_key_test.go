package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestAKeyOnlyInTheEnvironmentStaysOutOfTheEnvFile fails if a settings save
// of any field writes into .env a provider key only the environment holds (a
// credential broker's short-lived key would land on disk), or replaces the
// key .env holds with the environment's; or if a key the settings set is not
// written there.
func TestAKeyOnlyInTheEnvironmentStaysOutOfTheEnvFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path, envPath := filepath.Join(dir, "ghost-wispr.yaml"), filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(`silence_timeout: 30s
providers:
  deepgram: {api: deepgram, auth: {kind: api_key, env: GHOST_WISPR_DEEPGRAM_API_KEY}}
  openai: {api: openai, auth: {kind: api_key, env: GHOST_WISPR_OPENAI_API_KEY}}
  anthropic: {api: anthropic, auth: {kind: api_key, env: GHOST_WISPR_ANTHROPIC_API_KEY}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(EnvPrefix+"OPENAI_API_KEY"+"=sk-file\nOTHER_SETTING=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "dg-env") // the environment's alone
	t.Setenv(EnvPrefix+"OPENAI_API_KEY", "sk-env")   // overrides .env's
	store, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := store.Get(); cfg.APIKey("deepgram") != "dg-env" {
		got := cfg.APIKey("deepgram")
		t.Fatalf("deepgram's key = %q, want the environment's", got)
	}

	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}
	saved := loadEnvFile(envPath)
	if _, ok := saved[EnvPrefix+"DEEPGRAM_API_KEY"]; ok || saved[EnvPrefix+"OPENAI_API_KEY"] != "sk-file" || saved["OTHER_SETTING"] != "keep" {
		t.Fatalf("after a silence_timeout save .env holds %q; want no deepgram key, .env's own openai key and its other setting", saved)
	}

	if err := store.TryUpdate(func(c *Config) error { return c.SetAPIKey("anthropic", "ak-1") }); err != nil {
		t.Fatal(err)
	}
	if err := store.TryUpdate(func(c *Config) error { return c.SetAPIKey("deepgram", "dg-2") }); err != nil {
		t.Fatal(err)
	}
	saved = loadEnvFile(envPath)
	if saved[EnvPrefix+"ANTHROPIC_API_KEY"] != "ak-1" || saved[EnvPrefix+"DEEPGRAM_API_KEY"] != "dg-2" || saved[EnvPrefix+"OPENAI_API_KEY"] != "sk-file" {
		t.Fatalf("after two keys were set .env holds %q; want both, and .env's own openai key", saved)
	}
}

// TestAReadOnlyProviderKeyStaysByteIdenticalOnUnrelatedSettingsSave fails if
// a settings change that touches no provider key still calls writeEnvFile:
// reserializing the deployment-owned key would remove update.sh's comment or
// change its line. The saved silence timeout controls that the setting changed.
func TestAReadOnlyProviderKeyStaysByteIdenticalOnUnrelatedSettingsSave(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path, envPath := filepath.Join(dir, "ghost-wispr.yaml"), filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(`silence_timeout: 30s
providers:
  deepgram: {api: deepgram, auth: {kind: api_key, env: GHOST_WISPR_DEEPGRAM_API_KEY, read_only: true}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	const original = "# written by update.sh from Secret Manager\nGHOST_WISPR_DEEPGRAM_API_KEY=dg-deployed\nOTHER_SETTING=kept\n"
	if err := os.WriteFile(envPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	if got := cfg.APIKey("deepgram"); got != "dg-deployed" {
		t.Fatalf("deepgram's deployment key = %q, want dg-deployed", got)
	}

	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(envPath); err != nil || !bytes.Equal(after, []byte(original)) {
		t.Errorf(".env after an unrelated settings save = %q (%v), want it byte-identical to %q", after, err, original)
	}
	reopened, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().SilenceTimeout; got != "45s" {
		t.Errorf("silence_timeout after its save = %q, want 45s", got)
	}
}
