package config

import (
	"os"
	"path/filepath"
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
