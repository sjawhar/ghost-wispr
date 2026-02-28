# Config Panel Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a web UI settings panel for runtime config tuning — presets CRUD with test, silence timeout, transcription params, API keys, GDrive setup — with YAML/.env write-back.

**Architecture:** ConfigStore wraps Config with a mutex, validates mutations, writes YAML (non-secrets) and .env (secrets), and fires onChange callbacks. Three API endpoints: GET/PATCH /api/config + POST /api/config/presets/{name}/test. Svelte settings page at /settings route.

**Tech Stack:** Go 1.25, Svelte 5 (runes), TailwindCSS 4, Vitest, gopkg.in/yaml.v3

**Design doc:** `docs/plans/2026-02-28-config-panel-design.md`

---

### Task 1: ConfigStore — Core Get/Update/Write-Back

**Files:**
- Create: `internal/config/store.go`
- Test: `internal/config/store_test.go`

**Step 1: Write the failing tests**

```go
// store_test.go
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
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run TestNewStore -v && go test ./internal/config/ -run TestStore_ -v`
Expected: FAIL — `NewStore` not defined.

**Step 3: Implement ConfigStore**

```go
// store.go
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Store holds mutable config with thread-safe access and YAML write-back.
type Store struct {
	mu       sync.RWMutex
	cfg      Config
	path     string
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
	// Deep copy maps and slices.
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
// Returns an error if any field is invalid.
func (s *Store) validateForUpdate() error {
	if _, err := time.ParseDuration(s.cfg.SilenceTimeout); err != nil {
		return fmt.Errorf("invalid silence_timeout %q: %w", s.cfg.SilenceTimeout, err)
	}

	if s.cfg.Summarization.Model != "" {
		if _, _, err := parseModelInternal(s.cfg.Summarization.Model); err != nil {
			return err
		}
	}

	for name, preset := range s.cfg.Summarization.Presets {
		if preset.Model != "" {
			if _, _, err := parseModelInternal(preset.Model); err != nil {
				return fmt.Errorf("preset %q: %w", name, err)
			}
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
	return os.WriteFile(s.path, data, 0o644)
}

// parseModelInternal validates model format without importing llm package.
func parseModelInternal(model string) (string, string, error) {
	// Inline validation to avoid circular dependency with llm package.
	parts := splitModelString(model)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid model format %q: expected provider/model_name", model)
	}
	return parts[0], parts[1], nil
}

func splitModelString(model string) []string {
	for i, c := range model {
		if c == '/' {
			return []string{model[:i], model[i+1:]}
		}
	}
	return []string{model}
}
```

Note: `parseModelInternal` duplicates the simple split logic from `llm.ParseModel` to avoid a circular import. This is intentional — the config package shouldn't depend on llm.

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: All PASS.

**Step 5: Commit**

```bash
jj describe -m "feat: add ConfigStore with mutex-protected Get/Update and YAML write-back"
```

---

### Task 2: ConfigStore — .env Read/Write for Secrets

**Files:**
- Modify: `internal/config/store.go`
- Modify: `internal/config/config.go`
- Test: `internal/config/store_test.go` (add tests)

**Step 1: Write failing tests**

```go
func TestStore_Update_WritesSecretsToEnv(t *testing.T) {
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

func TestStore_LoadsSecretsFromEnvFile(t *testing.T) {
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

func TestStore_EnvVarOverridesEnvFile(t *testing.T) {
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
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run TestStore_.*Env -v`
Expected: FAIL — `NewStoreWithEnv` not defined.

**Step 3: Implement .env support**

Add to `store.go`:

```go
// NewStoreWithEnv loads config from YAML, then secrets from .env file
// (env vars still take precedence), and returns a mutable store.
func NewStoreWithEnv(yamlPath, envPath string) (*Store, []string, error) {
	// Load .env file first (into the config, not the process environment).
	envSecrets := loadEnvFile(envPath)

	cfg, warnings, err := Load(yamlPath)
	if err != nil {
		return nil, warnings, err
	}

	// Apply .env secrets where env var is not already set.
	applyEnvFileSecrets(&cfg, envSecrets)

	return &Store{cfg: cfg, path: yamlPath, envPath: envPath}, warnings, nil
}
```

Add `loadEnvFile`, `applyEnvFileSecrets`, and `writeEnvFile` functions. The .env format is simple `KEY=VALUE` lines — no need for a library.

Update `Update()` to also call `writeEnvFile` when secrets have changed.

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: All PASS.

**Step 5: Commit**

```bash
jj describe -m "feat: add .env read/write for API key persistence in ConfigStore"
```

---

### Task 3: API Endpoints — GET /api/config and PATCH /api/config

**Files:**
- Modify: `internal/server/api.go`
- Modify: `internal/server/server.go` (add ConfigStore to Handler params)
- Test: `internal/server/api_test.go` (add tests)

**Step 1: Write failing tests**

Add to `api_test.go`:

```go
func TestAPI_GetConfig(t *testing.T) {
	store := newTestConfigStore(t) // helper that creates a Store with test defaults
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{...}, ControlHooks{}, store)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if resp["silence_timeout"] != "30s" {
		t.Fatalf("expected silence_timeout 30s, got %v", resp["silence_timeout"])
	}

	// API keys should be booleans, not values
	keys := resp["api_keys"].(map[string]any)
	if _, ok := keys["openai"].(bool); !ok {
		t.Fatal("api_keys.openai should be a boolean")
	}
}

func TestAPI_PatchConfig_SilenceTimeout(t *testing.T) {
	store := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{...}, ControlHooks{}, store)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"silence_timeout": "45s"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	cfg := store.Get()
	if cfg.SilenceTimeout != "45s" {
		t.Fatalf("expected 45s after patch, got %q", cfg.SilenceTimeout)
	}
}

func TestAPI_PatchConfig_InvalidReturns400(t *testing.T) {
	store := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{...}, ControlHooks{}, store)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"silence_timeout": "not-valid"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestAPI_PatchConfig_AddPreset(t *testing.T) {
	store := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{...}, ControlHooks{}, store)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"summarization":{"presets":{"standup":{"description":"Stand-up","system_prompt":"Summarize briefly","user_template":"{{transcript}}"}}}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	cfg := store.Get()
	if _, ok := cfg.Summarization.Presets["standup"]; !ok {
		t.Fatal("expected standup preset to exist after patch")
	}
	if _, ok := cfg.Summarization.Presets["default"]; !ok {
		t.Fatal("default preset should still exist after adding standup")
	}
}

func TestAPI_PatchConfig_DeletePreset(t *testing.T) {
	store := newTestConfigStore(t)
	// Add a preset first
	_ = store.Update(func(c *Config) {
		c.Summarization.Presets["to-delete"] = Preset{
			Description: "temp", SystemPrompt: "x", UserTemplate: "{{transcript}}",
		}
	})

	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{...}, ControlHooks{}, store)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"summarization":{"presets":{"to-delete":null}}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	cfg := store.Get()
	if _, ok := cfg.Summarization.Presets["to-delete"]; ok {
		t.Fatal("to-delete preset should be removed after null patch")
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -run TestAPI_.*Config -v`
Expected: FAIL — new Handler signature not matched.

**Step 3: Implement the endpoints**

Add `*config.Store` parameter to `Handler()` and `registerAPIRoutes()`. Implement:

- `GET /api/config`: call `store.Get()`, build response struct that masks secrets to booleans, serialize.
- `PATCH /api/config`: decode JSON body into a `configPatch` struct using `json.RawMessage` for nested objects. Apply merge-patch semantics: for each field present, update the corresponding Config field. Use `store.Update()` which handles validation, write-back, and onChange.

The merge-patch for presets needs special handling: iterate the presets map, and for any key with a JSON `null` value, delete it from the config. For non-null values, merge fields into the existing preset (or create a new one).

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -v`
Expected: All PASS (both new and existing tests).

**Step 5: Commit**

```bash
jj describe -m "feat: add GET and PATCH /api/config endpoints with merge-patch semantics"
```

---

### Task 4: API Endpoint — POST /api/config/presets/{name}/test

**Files:**
- Modify: `internal/server/api.go`
- Modify: `internal/server/server.go`
- Test: `internal/server/api_test.go` (add tests)

**Step 1: Write failing tests**

```go
func TestAPI_TestPreset(t *testing.T) {
	// Setup store with a test preset
	store := newTestConfigStore(t)
	_ = store.Update(func(c *Config) {
		c.Summarization.Presets["test-preset"] = Preset{
			Description:  "Test",
			SystemPrompt: "Summarize",
			UserTemplate: "{{transcript}}",
		}
	})

	mockSummarize := func(ctx context.Context, preset Preset, model string, transcript string) (string, error) {
		return "## Test Summary", nil
	}

	h, err := Handler(testStaticFS(t), NewHub(), storeWithSegments, ControlHooks{}, store,
		WithTestSummarizer(mockSummarize))
	// ...

	body := `{"session_id": "s1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/presets/test-preset/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["summary"] != "## Test Summary" {
		t.Fatalf("expected test summary, got %v", resp["summary"])
	}
}

func TestAPI_TestPreset_UnknownPreset(t *testing.T) {
	store := newTestConfigStore(t)
	h, _ := Handler(testStaticFS(t), NewHub(), apiStoreStub{...}, ControlHooks{}, store)

	req := httptest.NewRequest(http.MethodPost, "/api/config/presets/nonexistent/test", strings.NewReader(`{"session_id":"s1"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}
```

**Step 2: Run tests to verify they fail**

**Step 3: Implement**

The test endpoint:
1. Look up the preset by name from `store.Get()`
2. Load segments for the session from the SessionStore
3. Build transcript text from segments
4. Call the summarizer with the preset's prompt/template (via a `TestSummarizer` func injected into ControlHooks)
5. Return `{"summary": "...", "error": ""}`

This is synchronous — the request blocks until the LLM returns. Set a generous timeout (60s) on the request context.

**Step 4: Run tests to verify they pass**

**Step 5: Commit**

```bash
jj describe -m "feat: add POST /api/config/presets/{name}/test for dry-run summarization"
```

---

### Task 5: Wire ConfigStore into main.go

**Files:**
- Modify: `cmd/ghost-wispr/main.go`

**Step 1: Replace `config.Load` with `config.NewStoreWithEnv`**

```go
// Before:
// cfg, cfgWarnings, err := config.Load(configPath)

// After:
envPath := filepath.Join(filepath.Dir(configPath), ".env")
store, cfgWarnings, err := config.NewStoreWithEnv(configPath, envPath)
if err != nil {
    log.Fatalf("config: %v", err)
}
cfg := store.Get()
```

**Step 2: Pass store to Handler**

Update the `server.Handler()` call to include the store.

**Step 3: Update clientFactory to read from store**

The `clientFactory` closure should call `store.Get()` to get current API keys rather than capturing the initial `apiKeys` map:

```go
clientFactory := func(provider, model string) (llm.Client, error) {
    cfg := store.Get()
    keys := map[string]string{
        "openai":    cfg.OpenAIAPIKey,
        "anthropic": cfg.AnthropicAPIKey,
        "gemini":    cfg.GeminiAPIKey,
    }
    key := keys[provider]
    if key == "" {
        return nil, fmt.Errorf("no API key for provider %q", provider)
    }
    var opts []llm.Option
    if provider == "openai" && cfg.Summarization.BaseURL != "" {
        opts = append(opts, llm.WithBaseURL(cfg.Summarization.BaseURL))
    }
    return llm.NewClient(provider, key, model, opts...)
}
```

**Step 4: Register onChange callbacks**

```go
store.OnChange(func(cfg config.Config) {
    detector.SetTimeout(cfg.ParsedSilenceTimeout())
    // Deepgram reconnect handled in Task 6
})
```

**Step 5: Verify existing tests still pass**

Run: `go test ./...`
Expected: All PASS.

**Step 6: Commit**

```bash
jj describe -m "refactor: wire ConfigStore into main.go, replace static config with live store"
```

---

### Task 6: Component Reactions — Detector Timeout and Deepgram Reconnect

**Files:**
- Modify: `internal/session/detector.go` (add SetTimeout method)
- Test: `internal/session/detector_test.go`
- Modify: `cmd/ghost-wispr/main.go` (Deepgram reconnect in onChange)

**Step 1: Write failing test for SetTimeout**

```go
func TestDetector_SetTimeout(t *testing.T) {
	d := NewDetector(30 * time.Second)

	d.SetTimeout(60 * time.Second)

	// Verify by triggering the timeout mechanism
	// (the existing OnUtteranceEnd test pattern works here)
}
```

**Step 2: Implement SetTimeout**

```go
func (d *Detector) SetTimeout(timeout time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if timeout > 0 {
		d.timeout = timeout
	}
}
```

**Step 3: Implement Deepgram reconnect in main.go**

Add a `reconnectDeepgram` function that:
1. Stops the current dgClient
2. Creates new client with updated transcription options from `store.Get()`
3. Connects and swaps the writer
4. Uses a mutex to protect the dgWriter swap

Wire this into the `store.OnChange` callback, checking if transcription settings actually changed before reconnecting.

**Step 4: Run tests**

Run: `go test ./...`
Expected: All PASS.

**Step 5: Commit**

```bash
jj describe -m "feat: add detector.SetTimeout and Deepgram reconnect on config change"
```

---

### Task 7: Frontend — Config API and State

**Files:**
- Create: `web/src/lib/config-api.ts`
- Create: `web/src/lib/config.svelte.ts`
- Test: `web/src/lib/__tests__/config-api.test.ts`

**Step 1: Write the config API module**

```typescript
// config-api.ts
export interface ConfigResponse {
  silence_timeout: string
  summarization: {
    model: string
    base_url: string
    presets: Record<string, PresetDetail>
  }
  transcription: {
    endpointing: string
    utterance_end_ms: string
  }
  gdrive: {
    folder_id: string
    has_credentials: boolean
  }
  api_keys: Record<string, boolean>
}

export interface PresetDetail {
  description: string
  system_prompt: string
  user_template: string
  model: string
}

export interface TestPresetResponse {
  summary: string
  error: string
}

export function fetchConfig(): Promise<ConfigResponse> { ... }
export function patchConfig(patch: Record<string, unknown>): Promise<ConfigResponse> { ... }
export function testPreset(name: string, sessionId: string): Promise<TestPresetResponse> { ... }
```

**Step 2: Write the config state module**

```typescript
// config.svelte.ts
import type { ConfigResponse } from './config-api'

type ConfigState = {
  loaded: boolean
  saving: boolean
  error: string
  config: ConfigResponse | null
}

export const configState = $state<ConfigState>({ ... })
export function setConfig(config: ConfigResponse): void { ... }
export function resetConfigState(): void { ... }
```

**Step 3: Write tests for the API module**

Test `fetchConfig`, `patchConfig`, `testPreset` using mocked fetch (matching the pattern in `web/src/lib/__tests__/api.test.ts`).

**Step 4: Run tests**

Run (from web/): `npm test`
Expected: All PASS.

**Step 5: Commit**

```bash
jj describe -m "feat: add config API client and state management for settings UI"
```

---

### Task 8: Frontend — Settings Page Shell and Navigation

**Files:**
- Create: `web/src/components/SettingsPage.svelte`
- Modify: `web/src/App.svelte` (add route toggle)
- Test: `web/src/components/__tests__/SettingsPage.test.ts`

**Step 1: Implement route switching in App.svelte**

Add a `currentView` state variable (`'main' | 'settings'`). Add a gear icon button in the header that toggles it. Conditionally render `SettingsPage` or the existing main content.

**Step 2: Create SettingsPage shell**

Three collapsible sections (empty for now — filled in subsequent tasks):
- Summarization Presets
- General Settings
- Integrations

Fetches config on mount via `fetchConfig()`.

**Step 3: Write tests**

Test that the gear icon toggles to settings view. Test that SettingsPage renders three section headings.

**Step 4: Run tests**

Run (from web/): `npm test`

**Step 5: Commit**

```bash
jj describe -m "feat: add settings page shell with route toggle in header"
```

---

### Task 9: Frontend — General Settings Section

**Files:**
- Create: `web/src/components/GeneralSettings.svelte`
- Test: `web/src/components/__tests__/GeneralSettings.test.ts`

**Step 1: Implement the component**

Inputs for:
- Silence timeout (text input, Go duration format)
- Default summarization model (text input, provider/model format)
- Transcription endpointing (number input)
- Transcription utterance_end_ms (number input)

Each field has inline validation. A "Save" button calls `patchConfig()` with changed fields only. Shows success/error feedback.

**Step 2: Write tests**

Test rendering with initial values. Test validation (invalid duration shows error). Test save calls patchConfig with correct payload.

**Step 3: Commit**

```bash
jj describe -m "feat: add General Settings section with silence timeout and transcription tuning"
```

---

### Task 10: Frontend — Presets CRUD Section

**Files:**
- Create: `web/src/components/PresetEditor.svelte`
- Create: `web/src/components/PresetList.svelte`
- Test: `web/src/components/__tests__/PresetEditor.test.ts`

**Step 1: Implement PresetList**

Shows all presets as cards. Click to expand into PresetEditor. "Add Preset" button creates a new empty editor.

**Step 2: Implement PresetEditor**

Full form: name (text, disabled for existing), description (text), system_prompt (textarea), user_template (textarea), model override (text, optional).

Save button calls `patchConfig()` with the preset nested under `summarization.presets`.
Delete button calls `patchConfig()` with `null` for the preset name. Disabled when name is "default".

**Step 3: Write tests**

Test expand/collapse. Test save sends correct patch. Test delete sends null. Test "default" preset can't be deleted.

**Step 4: Commit**

```bash
jj describe -m "feat: add preset list and editor components with CRUD via PATCH"
```

---

### Task 11: Frontend — Preset Test Modal

**Files:**
- Create: `web/src/components/PresetTestModal.svelte`
- Test: `web/src/components/__tests__/PresetTestModal.test.ts`

**Step 1: Implement the modal**

- Session picker dropdown (populated from `appState.dates` and session lists)
- "Run Test" button
- Spinner during LLM call
- Summary result rendered with svelte-markdown (already a dependency)
- Close button

Calls `testPreset(presetName, sessionId)` from config-api.ts.

**Step 2: Wire into PresetEditor**

Add a "Test" button to PresetEditor that opens the modal.

**Step 3: Write tests**

Test modal opens/closes. Test session selection. Test summary display after successful call. Test error display.

**Step 4: Commit**

```bash
jj describe -m "feat: add preset test modal with session picker and LLM dry-run"
```

---

### Task 12: Frontend — Integrations Section (API Keys + GDrive)

**Files:**
- Create: `web/src/components/IntegrationsSettings.svelte`
- Test: `web/src/components/__tests__/IntegrationsSettings.test.ts`

**Step 1: Implement API keys section**

One row per provider (Deepgram, OpenAI, Anthropic, Gemini):
- Green/gray dot showing `api_keys[provider]` boolean status
- Masked password input
- "Save" button per row, calls `patchConfig({"api_keys": {provider: value}})`

**Step 2: Implement GDrive section**

- Folder ID text input
- Credential file upload (reads file, base64-encodes, sends in patch as `gdrive.credentials_base64`)
- Status indicator from config (`has_credentials`)
- Save button

**Step 3: Write tests**

Test key status dots. Test save sends correct patch. Test credential upload encodes to base64.

**Step 4: Commit**

```bash
jj describe -m "feat: add integrations section with API key management and GDrive setup"
```

---

### Task 13: Build Verification and Polish

**Step 1: Run full backend tests**

Run: `go test ./...`
Expected: All PASS.

**Step 2: Run frontend checks**

Run (from web/): `npm run check && npm run lint && npm test`
Expected: All PASS.

**Step 3: Build the frontend**

Run (from web/): `npm run build`
Expected: Build succeeds, output in `cmd/ghost-wispr/static/`.

**Step 4: Build the Go binary**

Run: `go build ./cmd/ghost-wispr`
Expected: Compiles cleanly.

**Step 5: Commit**

```bash
jj describe -m "chore: verify full build passes with config panel feature"
```
