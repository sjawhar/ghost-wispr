package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNewStore_LoadsFromYAML(t *testing.T) {
	clearEnv(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("silence_timeout: 45s\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, _, err := NewStore(configPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	cfg := store.Get()
	if cfg.SilenceTimeout != "45s" {
		t.Fatalf("expected 45s, got %q", cfg.SilenceTimeout)
	}
}

func TestNewStore_DefaultsWhenNoFile(t *testing.T) {
	clearEnv(t)

	store, _, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	cfg := store.Get()
	if cfg.SilenceTimeout != "30s" {
		t.Fatalf("expected default 30s, got %q", cfg.SilenceTimeout)
	}
}

func TestStore_Update_ChangesConfig(t *testing.T) {
	clearEnv(t)
	store, _, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	if err := store.Update(func(c *Config) { c.SilenceTimeout = "60s" }); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	cfg := store.Get()
	if cfg.SilenceTimeout != "60s" {
		t.Fatalf("expected 60s after update, got %q", cfg.SilenceTimeout)
	}
}

func TestStore_Update_ValidationRejectsInvalid(t *testing.T) {
	clearEnv(t)
	store, _, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	err = store.Update(func(c *Config) { c.SilenceTimeout = "not-a-duration" })
	if err == nil {
		t.Fatal("expected validation error for invalid silence_timeout")
	}

	cfg := store.Get()
	if cfg.SilenceTimeout != "30s" {
		t.Fatalf("expected rollback to 30s, got %q", cfg.SilenceTimeout)
	}
}

func TestStore_Update_ValidationRejectsInvalidModel(t *testing.T) {
	clearEnv(t)
	store, _, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	err = store.Update(func(c *Config) { c.Summarization.Model = "no-slash" })
	if err == nil {
		t.Fatal("expected validation error for invalid model format")
	}

	cfg := store.Get()
	if cfg.Summarization.Model != "openai/gpt-4o-mini" {
		t.Fatalf("expected rollback to default model, got %q", cfg.Summarization.Model)
	}
}

func TestStore_Update_WritesYAML(t *testing.T) {
	clearEnv(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("silence_timeout: 30s\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, _, err := NewStore(configPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "45s") {
		t.Fatalf("expected YAML to contain 45s, got:\n%s", data)
	}
}

func TestStore_Update_KeywordsRoundTrip(t *testing.T) {
	clearEnv(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("transcription:\n  endpointing: \"400\"\n  utterance_end_ms: \"1000\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, _, err := NewStore(configPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	expected := []string{"Taiga", "Anthropic", "Lyon"}
	if err := store.Update(func(c *Config) {
		c.Transcription.Keywords = expected
	}); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	got := store.Get().Transcription.Keywords
	if len(got) != 3 || got[0] != "Taiga" || got[1] != "Anthropic" || got[2] != "Lyon" {
		t.Fatalf("expected keywords %v, got %v", expected, got)
	}

	reloaded, _, err := NewStore(configPath)
	if err != nil {
		t.Fatalf("reload store failed: %v", err)
	}

	reloadedKeywords := reloaded.Get().Transcription.Keywords
	if len(reloadedKeywords) != 3 || reloadedKeywords[0] != "Taiga" || reloadedKeywords[1] != "Anthropic" || reloadedKeywords[2] != "Lyon" {
		t.Fatalf("expected reloaded keywords %v, got %v", expected, reloadedKeywords)
	}
}

func TestStore_Update_DoesNotWriteSecretsToYAML(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"OPENAI_API_KEY", "sk-secret-key")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("silence_timeout: 30s\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, _, err := NewStore(configPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "sk-secret-key") {
		t.Fatal("secrets should not be written to YAML")
	}
}

func TestStore_Update_FiresOnChange(t *testing.T) {
	clearEnv(t)
	store, _, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	var called bool
	var calledCfg Config
	store.OnChange(func(c Config) {
		called = true
		calledCfg = c
	})

	if err := store.Update(func(c *Config) { c.SilenceTimeout = "60s" }); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	if !called {
		t.Fatal("onChange callback not called")
	}
	if calledCfg.SilenceTimeout != "60s" {
		t.Fatalf("onChange got wrong config, timeout=%q", calledCfg.SilenceTimeout)
	}
}

func TestStore_Update_DoesNotFireOnChangeOnFailure(t *testing.T) {
	clearEnv(t)
	store, _, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	var called bool
	store.OnChange(func(c Config) { called = true })

	_ = store.Update(func(c *Config) { c.SilenceTimeout = "bad" })

	if called {
		t.Fatal("onChange should not be called when update fails validation")
	}
}

func TestStore_Get_ReturnsCopy(t *testing.T) {
	clearEnv(t)
	store, _, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	cfg := store.Get()
	cfg.SilenceTimeout = "999s"

	cfg2 := store.Get()
	if cfg2.SilenceTimeout == "999s" {
		t.Fatal("Get should return a copy, not a reference")
	}
}

func TestStore_Get_ReturnsCopyOfPresets(t *testing.T) {
	clearEnv(t)
	store, _, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	cfg := store.Get()
	cfg.Summarization.Presets["injected"] = Preset{Description: "bad"}

	cfg2 := store.Get()
	if _, ok := cfg2.Summarization.Presets["injected"]; ok {
		t.Fatal("modifying returned preset map should not affect store")
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	clearEnv(t)
	store, _, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = store.Get()
		}()
		go func() {
			defer wg.Done()
			_ = store.Update(func(c *Config) { c.SilenceTimeout = "30s" })
		}()
	}
	wg.Wait()
}

func TestValidateModelFormat(t *testing.T) {
	tests := []struct {
		model string
		valid bool
	}{
		{"openai/gpt-4o", true},
		{"anthropic/claude-3-5-sonnet-latest", true},
		{"gemini/gemini-2.5-flash", true},
		{"no-slash", false},
		{"/no-provider", false},
		{"no-model/", false},
		{"", false},
	}

	for _, tt := range tests {
		err := validateModelFormat(tt.model)
		if tt.valid && err != nil {
			t.Errorf("expected %q to be valid, got error: %v", tt.model, err)
		}
		if !tt.valid && err == nil {
			t.Errorf("expected %q to be invalid, got nil error", tt.model)
		}
	}
}

func TestNewStoreWithEnv_LoadsSecretsFromEnvFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(configPath, []byte("silence_timeout: 30s\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	envContent := "GHOST_WISPR_OPENAI_API_KEY=sk-from-env-file\nGHOST_WISPR_DEEPGRAM_API_KEY=dg-key\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	store, _, err := NewStoreWithEnv(configPath, envPath)
	if err != nil {
		t.Fatalf("NewStoreWithEnv failed: %v", err)
	}

	cfg := store.Get()
	if cfg.OpenAIAPIKey != "sk-from-env-file" {
		t.Fatalf("expected key from .env file, got %q", cfg.OpenAIAPIKey)
	}
	if cfg.DeepgramAPIKey != "dg-key" {
		t.Fatalf("expected deepgram key from .env file, got %q", cfg.DeepgramAPIKey)
	}
}

func TestNewStoreWithEnv_EnvVarOverridesEnvFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	envContent := "GHOST_WISPR_OPENAI_API_KEY=from-file\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	t.Setenv(EnvPrefix+"OPENAI_API_KEY", "from-env-var")

	store, _, err := NewStoreWithEnv("", envPath)
	if err != nil {
		t.Fatalf("NewStoreWithEnv failed: %v", err)
	}

	cfg := store.Get()
	if cfg.OpenAIAPIKey != "from-env-var" {
		t.Fatalf("env var should override .env file, got %q", cfg.OpenAIAPIKey)
	}
}

func TestStore_Update_WritesSecretsToEnvFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(configPath, []byte("silence_timeout: 30s\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, _, err := NewStoreWithEnv(configPath, envPath)
	if err != nil {
		t.Fatalf("NewStoreWithEnv failed: %v", err)
	}

	if err := store.Update(func(c *Config) {
		c.OpenAIAPIKey = "sk-test-key"
	}); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !strings.Contains(string(data), "GHOST_WISPR_OPENAI_API_KEY=sk-test-key") {
		t.Fatalf("expected .env to contain OpenAI key, got:\n%s", data)
	}

	// Verify secret NOT in YAML
	yamlData, _ := os.ReadFile(configPath)
	if strings.Contains(string(yamlData), "sk-test-key") {
		t.Fatal("secret should not appear in YAML file")
	}
}

func TestStore_Update_PreservesExistingEnvEntries(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("GHOST_WISPR_DEEPGRAM_API_KEY=dg-existing\nCUSTOM_VAR=keep-me\n"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	store, _, err := NewStoreWithEnv("", envPath)
	if err != nil {
		t.Fatalf("NewStoreWithEnv failed: %v", err)
	}

	if err := store.Update(func(c *Config) {
		c.OpenAIAPIKey = "sk-new"
	}); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "CUSTOM_VAR=keep-me") {
		t.Fatalf("expected existing entries preserved, got:\n%s", content)
	}
	if !strings.Contains(content, "GHOST_WISPR_OPENAI_API_KEY=sk-new") {
		t.Fatalf("expected new key in .env, got:\n%s", content)
	}
}

func TestLoadEnvFile_HandlesComments(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "# This is a comment\nKEY1=val1\n\n  # Another comment\nKEY2=val2\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	result := loadEnvFile(envPath)
	if result["KEY1"] != "val1" {
		t.Fatalf("expected KEY1=val1, got %q", result["KEY1"])
	}
	if result["KEY2"] != "val2" {
		t.Fatalf("expected KEY2=val2, got %q", result["KEY2"])
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(result), result)
	}
}

func TestLoadEnvFile_HandlesQuotedValues(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "DOUBLE=\"sk-abc\"\nSINGLE='sk-def'\nUNQUOTED=sk-ghi\nEMPTY=\nMISMATCH=\"sk-jkl'\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	result := loadEnvFile(envPath)
	if result["DOUBLE"] != "sk-abc" {
		t.Fatalf("expected double-quoted value stripped, got %q", result["DOUBLE"])
	}
	if result["SINGLE"] != "sk-def" {
		t.Fatalf("expected single-quoted value stripped, got %q", result["SINGLE"])
	}
	if result["UNQUOTED"] != "sk-ghi" {
		t.Fatalf("expected unquoted value unchanged, got %q", result["UNQUOTED"])
	}
	if result["EMPTY"] != "" {
		t.Fatalf("expected empty value, got %q", result["EMPTY"])
	}
	if result["MISMATCH"] != "\"sk-jkl'" {
		t.Fatalf("expected mismatched quotes preserved, got %q", result["MISMATCH"])
	}
}

func TestWriteEnvFile_SortsKeysAlphabetically(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	// Write keys in reverse order
	initContent := "ZEBRA=z\nAPPLE=a\nMIDDLE=m\n"
	if err := os.WriteFile(envPath, []byte(initContent), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	store, _, err := NewStoreWithEnv("", envPath)
	if err != nil {
		t.Fatalf("NewStoreWithEnv failed: %v", err)
	}

	if err := store.Update(func(c *Config) {
		c.OpenAIAPIKey = "sk-sorted"
	}); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := 1; i < len(lines); i++ {
		prevKey := strings.SplitN(lines[i-1], "=", 2)[0]
		currKey := strings.SplitN(lines[i], "=", 2)[0]
		if prevKey > currKey {
			t.Fatalf("keys not sorted: %q appears before %q in:\n%s", prevKey, currKey, data)
		}
	}
}

func TestStore_Update_ValidationRejectsInvalidTranscription(t *testing.T) {
	clearEnv(t)

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid endpointing", func(c *Config) { c.Transcription.Endpointing = "400" }, false},
		{"valid zero endpointing", func(c *Config) { c.Transcription.Endpointing = "0" }, false},
		{"empty endpointing ok", func(c *Config) { c.Transcription.Endpointing = "" }, false},
		{"negative endpointing", func(c *Config) { c.Transcription.Endpointing = "-1" }, true},
		{"non-integer endpointing", func(c *Config) { c.Transcription.Endpointing = "abc" }, true},
		{"float endpointing", func(c *Config) { c.Transcription.Endpointing = "1.5" }, true},
		{"valid utterance_end_ms", func(c *Config) { c.Transcription.UtteranceEndMs = "1000" }, false},
		{"valid zero utterance_end_ms", func(c *Config) { c.Transcription.UtteranceEndMs = "0" }, false},
		{"empty utterance_end_ms ok", func(c *Config) { c.Transcription.UtteranceEndMs = "" }, false},
		{"negative utterance_end_ms", func(c *Config) { c.Transcription.UtteranceEndMs = "-100" }, true},
		{"non-integer utterance_end_ms", func(c *Config) { c.Transcription.UtteranceEndMs = "xyz" }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _, err := NewStore("")
			if err != nil {
				t.Fatalf("NewStore failed: %v", err)
			}
			err = store.Update(tt.mutate)
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
