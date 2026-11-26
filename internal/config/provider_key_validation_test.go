package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAProviderKeyCannotAddAnEnvSetting catches control characters in a key
// reaching writeEnvFile, where a newline would become another setting.
func TestAProviderKeyCannotAddAnEnvSetting(t *testing.T) {
	clearVoiceEnv(t)
	path := writeConfig(t, examplePi)
	envPath := filepath.Join(t.TempDir(), ".env")
	const original = "EXAMPLE_SETTING=kept\n"
	if err := os.WriteFile(envPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"dg-1\nEXAMPLE_SETTING=changed", "dg-1\rchanged", "dg-1\x00changed", "dg-1\tchanged"} {
		err := store.TryUpdate(func(c *Config) error { return c.SetAPIKey("deepgram", key) })
		if err == nil {
			t.Errorf("accepted key containing a control character: %q", key)
		}
		data, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != original || store.Get().ProviderKeys[EnvPrefix+"DEEPGRAM_API_KEY"] != "" {
			t.Fatalf("refused key changed the config or .env: %q", data)
		}
	}
	if err := store.TryUpdate(func(c *Config) error { return c.SetAPIKey("deepgram", " dg-2 ") }); err != nil {
		t.Fatalf("ordinary pasted key: %v", err)
	}
	reopened, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().ProviderKeys[EnvPrefix+"DEEPGRAM_API_KEY"]; got != "dg-2" {
		t.Fatalf("key after reload = %q, want dg-2", got)
	}
}

// TestADeploymentOwnedProviderKeyCannotBeSaved fails if SetAPIKey treats an
// api_key provider marked auth.read_only as writable: the deployment's Secret
// Manager key would reach .env. Its writable OpenAI control fails if the
// refusal applies to every api_key provider.
func TestADeploymentOwnedProviderKeyCannotBeSaved(t *testing.T) {
	clearVoiceEnv(t)
	path := writeConfig(t, `
providers:
  deepgram: {api: deepgram, auth: {kind: api_key, env: GHOST_WISPR_DEEPGRAM_API_KEY, read_only: true}}
  openai: {api: openai, auth: {kind: api_key, env: GHOST_WISPR_OPENAI_API_KEY}}
`)
	envPath := filepath.Join(t.TempDir(), ".env")
	const original = "UNCHANGED=kept\n"
	if err := os.WriteFile(envPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}

	err = store.TryUpdate(func(c *Config) error { return c.SetAPIKey("deepgram", "dg-replacement") })
	if err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Errorf("SetAPIKey(deepgram) = %v, want refusal naming read_only", err)
	}
	if got := store.Get().APIKey("deepgram"); got != "" {
		t.Errorf("deepgram's key after refusal = %q, want none", got)
	}
	if after, err := os.ReadFile(envPath); err != nil || string(after) != original {
		t.Errorf(".env after the refusal = %q (%v), want %q", after, err, original)
	}

	if err := store.TryUpdate(func(c *Config) error { return c.SetAPIKey("openai", "sk-writable") }); err != nil {
		t.Fatalf("SetAPIKey(openai): %v", err)
	}
	if got := store.Get().APIKey("openai"); got != "sk-writable" {
		t.Errorf("openai's key = %q, want sk-writable", got)
	}
	saved := loadEnvFile(envPath)
	if saved["UNCHANGED"] != "kept" || saved[EnvPrefix+"OPENAI_API_KEY"] != "sk-writable" || saved[EnvPrefix+"DEEPGRAM_API_KEY"] != "" {
		t.Errorf(".env after writable control = %q, want the kept setting and only the OpenAI key", saved)
	}
}
