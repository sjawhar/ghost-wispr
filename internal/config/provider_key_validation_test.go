package config

import (
	"os"
	"path/filepath"
	"slices"
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

// TestAReadOnlyProviderKeyIsRefused fails if SetAPIKey treats an api_key
// provider marked auth.read_only as writable: the deployment's Secret Manager
// key would reach .env. Its writable OpenAI control fails if the refusal
// applies to every api_key provider.
func TestAReadOnlyProviderKeyIsRefused(t *testing.T) {
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
	cfg := store.Get()
	if got := cfg.APIKey("deepgram"); got != "" {
		t.Errorf("deepgram's key after refusal = %q, want none", got)
	}
	if after, err := os.ReadFile(envPath); err != nil || string(after) != original {
		t.Errorf(".env after the refusal = %q (%v), want %q", after, err, original)
	}

	if err := store.TryUpdate(func(c *Config) error { return c.SetAPIKey("openai", "sk-writable") }); err != nil {
		t.Fatalf("SetAPIKey(openai): %v", err)
	}
	cfg = store.Get()
	if got := cfg.APIKey("openai"); got != "sk-writable" {
		t.Errorf("openai's key = %q, want sk-writable", got)
	}
	saved := loadEnvFile(envPath)
	if saved["UNCHANGED"] != "kept" || saved[EnvPrefix+"OPENAI_API_KEY"] != "sk-writable" || saved[EnvPrefix+"DEEPGRAM_API_KEY"] != "" {
		t.Errorf(".env after writable control = %q, want the kept setting and only the OpenAI key", saved)
	}
}

// TestAReadOnlyKeyIsRefusedWhateverItsProviderIsNamed fails if SetAPIKey
// decides a key is read-only by its provider's name, not by auth.read_only:
// here the read-only provider is speech, and the writable control is named
// deepgram, the name the Pi's read-only provider has.
func TestAReadOnlyKeyIsRefusedWhateverItsProviderIsNamed(t *testing.T) {
	clearVoiceEnv(t)
	path := writeConfig(t, `
providers:
  speech: {api: elevenlabs, auth: {kind: api_key, env: EXAMPLE_SPEECH_KEY, read_only: true}}
  deepgram: {api: deepgram, auth: {kind: api_key, env: EXAMPLE_DEEPGRAM_KEY}}
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

	err = store.TryUpdate(func(c *Config) error { return c.SetAPIKey("speech", "sk-replacement") })
	if err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Errorf("SetAPIKey(speech) = %v, want a refusal naming read_only", err)
	}
	cfg := store.Get()
	if got := cfg.APIKey("speech"); got != "" {
		t.Errorf("speech's key after the refusal = %q, want none", got)
	}
	if after, err := os.ReadFile(envPath); err != nil || string(after) != original {
		t.Errorf(".env after the refusal = %q (%v), want %q", after, err, original)
	}

	if err := store.TryUpdate(func(c *Config) error { return c.SetAPIKey("deepgram", "dg-writable") }); err != nil {
		t.Fatalf("SetAPIKey(deepgram), which has no read_only: %v", err)
	}
	saved := loadEnvFile(envPath)
	if saved["UNCHANGED"] != "kept" || saved["EXAMPLE_DEEPGRAM_KEY"] != "dg-writable" || saved["EXAMPLE_SPEECH_KEY"] != "" {
		t.Errorf(".env after the writable control = %q, want the kept setting and deepgram's key alone", saved)
	}
}

// TestReadOnlyOnAProviderWithNoKeyWarns fails if auth.read_only on a provider
// that signs in with no key starts without a warning that names it: there it
// does nothing, and nothing else would say so. It uses two such kinds, google
// and openai_wif, so a warning for one kind alone fails. The api_key provider
// beside them is the control: read_only does something there, so it must not
// warn.
func TestReadOnlyOnAProviderWithNoKeyWarns(t *testing.T) {
	clearVoiceEnv(t)
	_, warnings, err := Load(writeConfig(t, `
providers:
  vertex: {api: vertex, project: example-project, auth: {kind: google, read_only: true}}
  openai: {api: openai, auth: {kind: openai_wif, identity_provider_id: idp-example, service_account_id: sa-example, read_only: true}}
  deepgram: {api: deepgram, auth: {kind: api_key, env: EXAMPLE_DEEPGRAM_KEY, read_only: true}}
`))
	if err != nil {
		t.Fatal(err)
	}
	var readOnly []string
	for _, w := range warnings {
		if strings.Contains(w, "read_only") {
			readOnly = append(readOnly, w)
		}
	}
	names := func(prefix string) bool {
		return slices.ContainsFunc(readOnly, func(w string) bool { return strings.HasPrefix(w, prefix) })
	}
	if len(readOnly) != 2 || !names("providers.vertex.auth.read_only") || !names("providers.openai.auth.read_only") {
		t.Errorf("read_only warnings %q, want one for providers.vertex and one for providers.openai, and none for the api_key provider deepgram", readOnly)
	}
}
