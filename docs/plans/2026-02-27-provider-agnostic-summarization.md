# Provider-Agnostic Summarization Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace hardcoded OpenAI summarization with a provider-agnostic system supporting multiple LLM providers, configurable prompt presets, automatic preset routing, and re-summarization from the web UI.

**Architecture:** Thin `llm.Client` interface with per-provider SDK adapters (OpenAI, Anthropic, Gemini). Provider-agnostic `Summarizer` orchestrates preset selection (via LLM router when >1 preset), template rendering, and retries. Config uses `provider/model` format in YAML. New API endpoints enable re-summarization with preset selection from the frontend.

**Tech Stack:** Go 1.25, `github.com/sashabaranov/go-openai` (existing), `github.com/anthropics/anthropic-sdk-go` (new), `google.golang.org/genai` (new), Svelte 5, SQLite.

**Design Doc:** `.sisyphus/drafts/provider-agnostic-summarization.md`

---

### Task 1: LLM Client Interface + OpenAI Implementation

**Files:**
- Create: `internal/llm/llm.go`
- Create: `internal/llm/openai.go`
- Create: `internal/llm/openai_test.go`

**Step 1: Write the interface and model parser**

`internal/llm/llm.go`:
```go
package llm

import (
	"context"
	"fmt"
	"strings"
)

// Message represents a chat message with a role and content.
type Message struct {
	Role    string // "system", "user", "assistant"
	Content string
}

// Client is the interface all LLM provider implementations must satisfy.
type Client interface {
	Complete(ctx context.Context, messages []Message) (string, error)
}

// Option configures a Client.
type Option func(*clientOptions)

type clientOptions struct {
	baseURL string
}

// WithBaseURL sets a custom base URL (for OpenAI-compatible endpoints).
func WithBaseURL(url string) Option {
	return func(o *clientOptions) {
		o.baseURL = url
	}
}

// ParseModel splits "provider/model_name" into provider and model.
// Returns an error if the format is invalid.
func ParseModel(model string) (provider, modelName string, err error) {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid model format %q: expected provider/model_name", model)
	}
	return parts[0], parts[1], nil
}

// NewClient creates a Client for the given provider.
func NewClient(provider, apiKey, model string, opts ...Option) (Client, error) {
	o := &clientOptions{}
	for _, opt := range opts {
		opt(o)
	}

	switch provider {
	case "openai":
		return newOpenAIClient(apiKey, model, o)
	case "anthropic":
		return newAnthropicClient(apiKey, model, o)
	case "gemini":
		return newGeminiClient(apiKey, model, o)
	default:
		return nil, fmt.Errorf("unknown LLM provider %q: supported providers are openai, anthropic, gemini", provider)
	}
}
```

**Step 2: Write failing test for OpenAI client**

`internal/llm/openai_test.go`:
```go
package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if body["model"] != "gpt-4o-mini" {
			t.Fatalf("expected model gpt-4o-mini, got %v", body["model"])
		}

		msgs := body["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(msgs))
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "test summary"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client, err := NewClient("openai", "test-key", "gpt-4o-mini", WithBaseURL(server.URL+"/v1"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	got, err := client.Complete(context.Background(), []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "test summary" {
		t.Fatalf("expected 'test summary', got %q", got)
	}
}

func TestOpenAICompleteWithCustomBaseURL(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"content": "ok"},
			}},
		})
	}))
	defer server.Close()

	client, err := NewClient("openai", "test-key", "local-model", WithBaseURL(server.URL+"/v1"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected /v1/chat/completions, got %q", gotPath)
	}
}
```

**Step 3: Run test to verify it fails**

Run: `go test ./internal/llm/ -count=1 -v`
Expected: FAIL — `newOpenAIClient` not defined

**Step 4: Implement OpenAI client**

`internal/llm/openai.go`:
```go
package llm

import (
	"context"
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

type openaiClient struct {
	client *openai.Client
	model  string
}

func newOpenAIClient(apiKey, model string, opts *clientOptions) (*openaiClient, error) {
	config := openai.DefaultConfig(apiKey)
	if opts.baseURL != "" {
		config.BaseURL = opts.baseURL
	}
	return &openaiClient{
		client: openai.NewClientWithConfig(config),
		model:  model,
	}, nil
}

func (c *openaiClient) Complete(ctx context.Context, messages []Message) (string, error) {
	msgs := make([]openai.ChatCompletionMessage, len(messages))
	for i, m := range messages {
		msgs[i] = openai.ChatCompletionMessage{Role: m.Role, Content: m.Content}
	}

	resp, err := c.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: msgs,
	})
	if err != nil {
		return "", fmt.Errorf("openai completion: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", nil
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}
```

**Step 5: Run tests to verify they pass**

Run: `go test ./internal/llm/ -count=1 -v`
Expected: PASS

Note: The `newAnthropicClient` and `newGeminiClient` stubs are needed for `llm.go` to compile. Add temporary stubs that return `fmt.Errorf("not implemented")` — these get replaced in Tasks 2 and 3.

**Step 6: Commit**

Message: `feat(llm): add Client interface and OpenAI implementation`

---

### Task 2: Anthropic Implementation

**Files:**
- Create: `internal/llm/anthropic.go`
- Create: `internal/llm/anthropic_test.go`

**Step 1: Add dependency**

Run: `go get github.com/anthropics/anthropic-sdk-go`

**Step 2: Write failing test**

`internal/llm/anthropic_test.go`:
```go
package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if body["model"] != "claude-sonnet-4-20250514" {
			t.Fatalf("expected model claude-sonnet-4-20250514, got %v", body["model"])
		}

		// Verify system prompt is present
		system := body["system"]
		if system == nil {
			t.Fatal("expected system prompt")
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg_1",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "text",
				"text": "anthropic summary",
			}},
			"stop_reason": "end_turn",
			"model":       "claude-sonnet-4-20250514",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer server.Close()

	client, err := NewClient("anthropic", "test-key", "claude-sonnet-4-20250514",
		WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	got, err := client.Complete(context.Background(), []Message{
		{Role: "system", Content: "Be concise."},
		{Role: "user", Content: "Hello"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "anthropic summary" {
		t.Fatalf("expected 'anthropic summary', got %q", got)
	}
}
```

**Step 3: Run test to verify it fails**

Run: `go test ./internal/llm/ -run TestAnthropic -count=1 -v`
Expected: FAIL

**Step 4: Implement Anthropic client**

`internal/llm/anthropic.go`:

The Anthropic SDK separates system prompts from messages. Extract any "system" role messages from the input, pass them as `System` in params, and send the rest as `Messages`.

```go
package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type anthropicClient struct {
	client *anthropic.Client
	model  string
}

func newAnthropicClient(apiKey, model string, opts *clientOptions) (*anthropicClient, error) {
	clientOpts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if opts.baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.baseURL))
	}
	return &anthropicClient{
		client: anthropic.NewClient(clientOpts...),
		model:  model,
	}, nil
}

func (c *anthropicClient) Complete(ctx context.Context, messages []Message) (string, error) {
	var systemBlocks []anthropic.TextBlockParam
	var chatMessages []anthropic.MessageParam

	for _, m := range messages {
		switch m.Role {
		case "system":
			systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: m.Content})
		case "user":
			chatMessages = append(chatMessages, anthropic.NewUserMessage(
				anthropic.NewTextBlock(m.Content),
			))
		case "assistant":
			chatMessages = append(chatMessages, anthropic.NewAssistantMessage(
				anthropic.NewTextBlock(m.Content),
			))
		}
	}

	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: 4096,
		System:    systemBlocks,
		Messages:  chatMessages,
	})
	if err != nil {
		return "", fmt.Errorf("anthropic completion: %w", err)
	}

	var b strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return strings.TrimSpace(b.String()), nil
}
```

**Step 5: Run tests**

Run: `go test ./internal/llm/ -count=1 -v`
Expected: PASS

**Step 6: Commit**

Message: `feat(llm): add Anthropic provider implementation`

---

### Task 3: Gemini Implementation

**Files:**
- Create: `internal/llm/gemini.go`
- Create: `internal/llm/gemini_test.go`

**Step 1: Add dependency**

Run: `go get google.golang.org/genai`

**Step 2: Write failing test**

`internal/llm/gemini_test.go`:

The `google.golang.org/genai` client uses HTTP under the hood. We can test it by pointing at a local httptest server. However, the genai SDK may not support custom base URLs easily. If it doesn't, test at a higher level with an interface mock. Check the SDK docs for `option.WithEndpoint` or similar.

Alternative: test the message conversion logic in isolation and use a thin integration test.

```go
package llm

import (
	"context"
	"testing"
)

func TestGeminiMessageConversion(t *testing.T) {
	// Test that system messages are extracted and user/assistant messages are converted correctly
	messages := []Message{
		{Role: "system", Content: "Be concise."},
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there"},
		{Role: "user", Content: "Summarize this"},
	}

	systemInstr, parts := convertGeminiMessages(messages)
	if systemInstr == nil || systemInstr.Parts[0].Text != "Be concise." {
		t.Fatal("expected system instruction")
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 content parts, got %d", len(parts))
	}
	if parts[0].Role != "user" {
		t.Fatalf("expected first part role 'user', got %q", parts[0].Role)
	}
}
```

**Step 3: Run test to verify it fails**

Run: `go test ./internal/llm/ -run TestGemini -count=1 -v`
Expected: FAIL

**Step 4: Implement Gemini client**

`internal/llm/gemini.go`:

```go
package llm

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

type geminiClient struct {
	client *genai.Client
	model  string
}

func newGeminiClient(apiKey, model string, opts *clientOptions) (*geminiClient, error) {
	ctx := context.Background()
	cfg := &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGoogleAI,
	}
	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}
	return &geminiClient{client: client, model: model}, nil
}

// convertGeminiMessages extracts system instruction and converts messages to genai.Content.
func convertGeminiMessages(messages []Message) (*genai.Content, []*genai.Content) {
	var systemInstruction *genai.Content
	var contents []*genai.Content

	for _, m := range messages {
		switch m.Role {
		case "system":
			systemInstruction = &genai.Content{
				Parts: []*genai.Part{{Text: m.Content}},
			}
		case "user":
			contents = append(contents, &genai.Content{
				Role:  "user",
				Parts: []*genai.Part{{Text: m.Content}},
			})
		case "assistant":
			contents = append(contents, &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: m.Content}},
			})
		}
	}
	return systemInstruction, contents
}

func (c *geminiClient) Complete(ctx context.Context, messages []Message) (string, error) {
	systemInstruction, contents := convertGeminiMessages(messages)

	config := &genai.GenerateContentConfig{
		SystemInstruction: systemInstruction,
	}

	// The genai SDK expects the last user message as the prompt,
	// and prior messages as history. For our simple case, we can
	// use GenerateContent with all contents.
	var lastContent *genai.Content
	if len(contents) > 0 {
		lastContent = contents[len(contents)-1]
	}
	if lastContent == nil {
		return "", fmt.Errorf("gemini: no user message provided")
	}

	// Build parts from the last content
	result, err := c.client.Models.GenerateContent(ctx, c.model, lastContent.Parts[0], config)
	if err != nil {
		return "", fmt.Errorf("gemini completion: %w", err)
	}

	return strings.TrimSpace(result.Text()), nil
}
```

Note: The Gemini implementation may need refinement based on actual SDK behavior with multi-turn. For our summarization use case, it's always system + single user message, so this is sufficient.

**Step 5: Run tests**

Run: `go test ./internal/llm/ -count=1 -v`
Expected: PASS

**Step 6: Commit**

Message: `feat(llm): add Gemini provider implementation`

---

### Task 4: ParseModel + NewClient Tests

**Files:**
- Create: `internal/llm/llm_test.go`

**Step 1: Write tests**

`internal/llm/llm_test.go`:
```go
package llm

import "testing"

func TestParseModel(t *testing.T) {
	tests := []struct {
		input    string
		provider string
		model    string
		wantErr  bool
	}{
		{"openai/gpt-4o-mini", "openai", "gpt-4o-mini", false},
		{"anthropic/claude-sonnet-4-20250514", "anthropic", "claude-sonnet-4-20250514", false},
		{"gemini/gemini-2.0-flash", "gemini", "gemini-2.0-flash", false},
		{"openai/ft:gpt-4o:custom:id", "openai", "ft:gpt-4o:custom:id", false},
		{"noslash", "", "", true},
		{"/no-provider", "", "", true},
		{"no-model/", "", "", true},
		{"", "", "", true},
	}

	for _, tt := range tests {
		provider, model, err := ParseModel(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseModel(%q) expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseModel(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if provider != tt.provider || model != tt.model {
			t.Errorf("ParseModel(%q) = (%q, %q), want (%q, %q)", tt.input, provider, model, tt.provider, tt.model)
		}
	}
}

func TestNewClientUnknownProvider(t *testing.T) {
	_, err := NewClient("unknown", "key", "model")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
```

**Step 2: Run tests**

Run: `go test ./internal/llm/ -count=1 -v`
Expected: PASS

**Step 3: Commit**

Message: `test(llm): add ParseModel and registry tests`

---

### Task 5: Config Changes

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Step 1: Write failing tests for new config fields**

Add to `config_test.go`:
```go
func TestLoadSummarizationPresets(t *testing.T) {
	yaml := `
summarization:
  model: openai/gpt-4o-mini
  presets:
    default:
      description: "General summary"
      system_prompt: "Summarize this."
      user_template: "{{transcript}}"
    detailed:
      description: "Detailed analysis"
      system_prompt: "Analyze in detail."
      user_template: "Meeting on {{date}}:\n\n{{transcript}}"
      model: anthropic/claude-sonnet-4-20250514
`
	// Write yaml to temp file, call Load, verify Summarization struct
}

func TestLoadSummarizationModelEnvOverride(t *testing.T) {
	t.Setenv("GHOST_WISPR_SUMMARIZATION_MODEL", "anthropic/claude-sonnet-4-20250514")
	// Load config, verify Summarization.Model is overridden
}
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -count=1 -v`

**Step 3: Implement config changes**

Update `Config` struct in `config.go`:
```go
type Preset struct {
	Description  string `yaml:"description"`
	SystemPrompt string `yaml:"system_prompt"`
	UserTemplate string `yaml:"user_template"`
	Model        string `yaml:"model"` // optional override, format: provider/model_name
}

type Summarization struct {
	Model   string            `yaml:"model"`    // format: provider/model_name
	BaseURL string            `yaml:"base_url"` // optional, for OpenAI-compatible
	Presets map[string]Preset `yaml:"presets"`
}

type Config struct {
	DBPath                string        `yaml:"db_path"`
	AudioDir              string        `yaml:"audio_dir"`
	SilenceTimeout        string        `yaml:"silence_timeout"`
	MicSampleRate         int           `yaml:"mic_sample_rate"`
	MicSampleRates        []int         `yaml:"mic_sample_rates"`
	Summarization         Summarization `yaml:"summarization"`
	GDriveFolderID        string        `yaml:"gdrive_folder_id"`
	GoogleCredentialsFile string        `yaml:"google_credentials_file"`

	// Secrets — env vars only, never serialized to YAML.
	DeepgramAPIKey  string `yaml:"-"`
	OpenAIAPIKey    string `yaml:"-"`
	AnthropicAPIKey string `yaml:"-"`
	GeminiAPIKey    string `yaml:"-"`
}
```

Remove `OpenAIModel` field. Update `defaults()` to set default summarization:
```go
Summarization: Summarization{
	Model: "openai/gpt-4o-mini",
	Presets: map[string]Preset{
		"default": {
			Description:  "General-purpose meeting summary with key topics, decisions, and action items",
			SystemPrompt: "Summarize the following office conversation transcript concisely in markdown. Include key topics, decisions made, and action items if any.",
			UserTemplate: "{{transcript}}",
		},
	},
},
```

Update `applyEnvOverrides`:
- Remove `GHOST_WISPR_OPENAI_MODEL`
- Add `GHOST_WISPR_SUMMARIZATION_MODEL` → `cfg.Summarization.Model`

Update `loadSecrets`:
- Add `cfg.AnthropicAPIKey = os.Getenv(EnvPrefix + "ANTHROPIC_API_KEY")`
- Add `cfg.GeminiAPIKey = os.Getenv(EnvPrefix + "GEMINI_API_KEY")`

Update `validate`:
- Remove old OpenAI key warning
- Add: for each unique provider referenced in presets, check if the corresponding API key is set
- Add: validate that a `default` preset exists
- Add: validate `ParseModel` format for all model strings

**Step 4: Run tests**

Run: `go test ./internal/config/ -count=1 -v`
Expected: PASS

**Step 5: Commit**

Message: `feat(config): add summarization presets and multi-provider API keys`

---

### Task 6: DB Migration — Add summary_preset Column

**Files:**
- Modify: `internal/storage/sqlite.go`
- Modify: `internal/storage/sqlite_test.go`

**Step 1: Write failing test**

```go
func TestUpdateSummaryWithPreset(t *testing.T) {
	store := newTestStore(t)
	store.CreateSession("s1", time.Now())
	err := store.UpdateSummary("s1", "text", "completed", "detailed")
	if err != nil {
		t.Fatalf("UpdateSummary: %v", err)
	}
	sess, _ := store.GetSession("s1")
	if sess.SummaryPreset != "detailed" {
		t.Fatalf("expected preset 'detailed', got %q", sess.SummaryPreset)
	}
}
```

**Step 2: Run test to verify it fails**

**Step 3: Implement changes**

Add to `Session` struct:
```go
SummaryPreset string `json:"summary_preset"`
```

Add column to `init()`:
```sql
ALTER TABLE sessions ADD COLUMN summary_preset TEXT NOT NULL DEFAULT '';
```

Use `ALTER TABLE` wrapped in a check: query `PRAGMA table_info(sessions)` first, add column only if missing. Or simpler: use `CREATE TABLE IF NOT EXISTS` with the new column (since this is the schema definition, just add it there and handle existing DBs with a migration block).

Update `UpdateSummary` signature to accept preset:
```go
func (s *SQLiteStore) UpdateSummary(sessionID, summary, status, preset string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET summary = ?, summary_status = ?, summary_preset = ? WHERE id = ?`,
		summary, status, preset, sessionID,
	)
	// ...
}
```

Update `scanSessions` and `GetSession` to scan `summary_preset`.

Update the `session.Store` interface in `internal/session/types.go`:
```go
UpdateSummary(sessionID, summary, status, preset string) error
```

Update all callers of `UpdateSummary` in `internal/session/manager.go` to pass preset (empty string `""` for failure cases, actual preset name for success).

Update `storeMock` in `internal/session/manager_test.go` to match the new 4-param signature.

**Step 4: Run tests**

Run: `go test ./internal/storage/ ./internal/session/ -count=1 -v`
Expected: PASS

**Step 5: Commit**

Message: `feat(storage): add summary_preset column and update interfaces`

---

### Task 7: Summarizer Orchestrator

**Files:**
- Create: `internal/summary/summarizer.go`
- Create: `internal/summary/summarizer_test.go`
- Delete: `internal/summary/openai.go`
- Delete: `internal/summary/openai_test.go`

**Step 1: Write failing tests for the new summarizer**

`internal/summary/summarizer_test.go`:

Test cases:
1. `TestSummarizeSinglePreset` — one preset, no router, renders template, calls LLM
2. `TestSummarizeSkipsShortTranscript` — <20 words, returns empty
3. `TestSummarizeRendersTemplate` — verifies `{{transcript}}` and `{{date}}` substitution
4. `TestSummarizeWithPreset` — specific preset bypasses router
5. `TestSummarizeRetries` — retries on failure with backoff

Use a mock `llm.Client`:
```go
type mockLLMClient struct {
	calls    int
	response string
	err      error
}

func (m *mockLLMClient) Complete(_ context.Context, messages []llm.Message) (string, error) {
	m.calls++
	if m.err != nil && m.calls < 3 {
		return "", m.err
	}
	return m.response, nil
}
```

The `Summarizer` needs a `ClientFactory` function rather than a single `Client`, since different presets may use different providers:
```go
type ClientFactory func(provider, model string) (llm.Client, error)
```

**Step 2: Run tests to verify they fail**

**Step 3: Implement summarizer**

`internal/summary/summarizer.go`:
```go
package summary

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

// ClientFactory creates an llm.Client for a given provider and model.
type ClientFactory func(provider, model string) (llm.Client, error)

// IdempotencyStore gates duplicate summary requests.
type IdempotencyStore interface {
	ClaimSummaryRequest(sessionID, promptHash string) (bool, error)
}

type Summarizer struct {
	cfg     config.Summarization
	factory ClientFactory
	store   IdempotencyStore
	router  *Router
	sleep   func(time.Duration)
}

func New(cfg config.Summarization, factory ClientFactory, store IdempotencyStore) *Summarizer {
	var router *Router
	if len(cfg.Presets) > 1 {
		router = NewRouter(cfg, factory)
	}
	return &Summarizer{
		cfg:     cfg,
		factory: factory,
		store:   store,
		router:  router,
		sleep:   time.Sleep,
	}
}

// Summarize implements session.Summarizer. Returns (summary, presetName, error).
func (s *Summarizer) Summarize(ctx context.Context, sessionID, transcript string) (string, error) {
	presetName, err := s.selectPreset(ctx, transcript)
	if err != nil {
		return "", fmt.Errorf("select preset: %w", err)
	}
	return s.SummarizeWithPreset(ctx, sessionID, transcript, presetName)
}

// SummarizeWithPreset runs summarization with a specific named preset.
func (s *Summarizer) SummarizeWithPreset(ctx context.Context, sessionID, transcript, presetName string) (string, error) {
	if len(strings.Fields(transcript)) < 20 {
		return "", nil
	}

	preset, ok := s.cfg.Presets[presetName]
	if !ok {
		return "", fmt.Errorf("unknown preset %q", presetName)
	}

	// Resolve model: preset override or global default
	modelStr := preset.Model
	if modelStr == "" {
		modelStr = s.cfg.Model
	}

	provider, model, err := llm.ParseModel(modelStr)
	if err != nil {
		return "", err
	}

	client, err := s.factory(provider, model)
	if err != nil {
		return "", fmt.Errorf("create llm client: %w", err)
	}

	// Render templates
	date := time.Now().UTC().Format("2006-01-02")
	userContent := strings.ReplaceAll(preset.UserTemplate, "{{transcript}}", transcript)
	userContent = strings.ReplaceAll(userContent, "{{date}}", date)

	messages := []llm.Message{
		{Role: "system", Content: preset.SystemPrompt},
		{Role: "user", Content: userContent},
	}

	// Retry with backoff
	backoff := []time.Duration{1 * time.Second, 4 * time.Second, 16 * time.Second}
	var lastErr error
	for attempt := range backoff {
		result, err := client.Complete(ctx, messages)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt < len(backoff)-1 {
			s.sleep(backoff[attempt])
		}
	}
	return "", fmt.Errorf("summarize failed after retries: %w", lastErr)
}

func (s *Summarizer) selectPreset(ctx context.Context, transcript string) (string, error) {
	if s.router == nil {
		// Single preset — return its name
		for name := range s.cfg.Presets {
			return name, nil
		}
		return "default", nil
	}
	return s.router.SelectPreset(ctx, transcript)
}

// PresetUsed returns the preset name that was used. The caller (manager.go)
// needs this to store in the DB. We solve this by having SummarizeWithPreset
// be the public API that manager calls after getting the preset name.
// The Summarize method is a convenience that auto-selects.

// Presets returns the configured presets (for the API).
func (s *Summarizer) Presets() map[string]config.Preset {
	return s.cfg.Presets
}
```

**Step 4: Run tests**

Run: `go test ./internal/summary/ -count=1 -v`
Expected: PASS

**Step 5: Delete old files**

Delete `internal/summary/openai.go` and `internal/summary/openai_test.go`.

**Step 6: Commit**

Message: `feat(summary): provider-agnostic summarizer with preset support`

---

### Task 8: Router — Auto-Select Preset via LLM

**Files:**
- Create: `internal/summary/router.go`
- Create: `internal/summary/router_test.go`

**Step 1: Write failing tests**

Test cases:
1. `TestRouterSelectsCorrectPreset` — mock LLM returns a valid preset name
2. `TestRouterFallsBackToDefault` — mock LLM returns garbage, router returns "default"
3. `TestSampleTranscript` — verifies the sampling logic (first 300 + middle 200 + last 200 words)

**Step 2: Implement router**

`internal/summary/router.go`:
```go
package summary

import (
	"context"
	"fmt"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

type Router struct {
	cfg     config.Summarization
	factory ClientFactory
}

func NewRouter(cfg config.Summarization, factory ClientFactory) *Router {
	return &Router{cfg: cfg, factory: factory}
}

// SampleTranscript extracts ~700 words: first 300, middle 200, last 200.
func SampleTranscript(transcript string, firstN, midN, lastN int) string {
	words := strings.Fields(transcript)
	total := len(words)

	if total <= firstN+midN+lastN {
		return transcript
	}

	first := strings.Join(words[:firstN], " ")

	midStart := (total - midN) / 2
	mid := strings.Join(words[midStart:midStart+midN], " ")

	last := strings.Join(words[total-lastN:], " ")

	return first + "\n\n[...]\n\n" + mid + "\n\n[...]\n\n" + last
}

func (r *Router) SelectPreset(ctx context.Context, transcript string) (string, error) {
	sampled := SampleTranscript(transcript, 300, 200, 200)

	// Build preset descriptions
	var presetList strings.Builder
	for name, preset := range r.cfg.Presets {
		fmt.Fprintf(&presetList, "- %s: %s\n", name, preset.Description)
	}

	prompt := fmt.Sprintf(`Given this conversation excerpt, choose the single best summarization preset.

Conversation excerpt:
%s

Available presets:
%s
Reply with ONLY the preset name, nothing else.`, sampled, presetList.String())

	provider, model, err := llm.ParseModel(r.cfg.Model)
	if err != nil {
		return "default", nil
	}

	client, err := r.factory(provider, model)
	if err != nil {
		return "default", nil
	}

	result, err := client.Complete(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return "default", nil
	}

	chosen := strings.TrimSpace(result)
	if _, ok := r.cfg.Presets[chosen]; ok {
		return chosen, nil
	}
	return "default", nil
}
```

**Step 3: Run tests**

Run: `go test ./internal/summary/ -count=1 -v`
Expected: PASS

**Step 4: Commit**

Message: `feat(summary): add LLM-based preset router with transcript sampling`

---

### Task 9: API Endpoints

**Files:**
- Modify: `internal/server/api.go`
- Modify: `internal/server/api_test.go`
- Modify: `internal/server/server.go`

**Step 1: Write failing tests**

Add to `api_test.go`:
```go
func TestGetPresets(t *testing.T) {
	// Hit GET /api/presets, verify response is a map of preset names + descriptions
}

func TestResummarize(t *testing.T) {
	// Hit POST /api/sessions/{id}/resummarize with {"preset": "detailed"}
	// Verify it triggers summarization and returns 202 Accepted
}

func TestResummarizeWithoutPreset(t *testing.T) {
	// Hit POST /api/sessions/{id}/resummarize with no body
	// Verify it still works (router picks preset)
}
```

**Step 2: Implement endpoints**

The server needs access to the `Summarizer` (for presets list and re-summarization) and the `Store` (for loading segments). Add a `SummarizationHooks` struct to `ControlHooks` or pass the summarizer directly.

Add to `server.go` `ControlHooks`:
```go
type ResummarizeFunc func(ctx context.Context, sessionID, preset string) error

type ControlHooks struct {
	// ... existing fields ...
	Presets     func() map[string]config.Preset
	Resummarize ResummarizeFunc
}
```

Add to `api.go`:
```go
mux.HandleFunc("GET /api/presets", func(w http.ResponseWriter, r *http.Request) {
	if controls.Presets == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	presets := controls.Presets()
	// Return just names + descriptions (don't expose full prompts)
	result := make(map[string]string, len(presets))
	for name, p := range presets {
		result[name] = p.Description
	}
	writeJSON(w, http.StatusOK, result)
})

mux.HandleFunc("POST /api/sessions/{id}/resummarize", func(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if !validSessionID(sessionID) {
		writeJSONError(w, http.StatusForbidden, "invalid session id")
		return
	}

	var body struct {
		Preset string `json:"preset"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	if controls.Resummarize == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "summarization not configured")
		return
	}

	// Run async — return 202 immediately, result comes via WebSocket
	go func() {
		_ = controls.Resummarize(context.Background(), sessionID, body.Preset)
	}()

	w.WriteHeader(http.StatusAccepted)
})
```

**Step 3: Run tests**

Run: `go test ./internal/server/ -count=1 -v`
Expected: PASS

**Step 4: Commit**

Message: `feat(server): add presets and resummarize API endpoints`

---

### Task 10: Frontend — Resummarize UI

**Files:**
- Modify: `web/src/lib/types.ts` — add `PresetMap` type
- Modify: `web/src/lib/api.ts` or create `web/src/lib/api.ts` — add `fetchPresets()`, `resummarize()` functions
- Modify: `web/src/lib/state.svelte.ts` — add presets to app state
- Modify session card component — add resummarize button + preset dropdown
- Add/modify tests

**Step 1: Add types**

```typescript
export type PresetMap = Record<string, string>; // name → description
```

**Step 2: Add API functions**

```typescript
export async function fetchPresets(): Promise<PresetMap> {
	const res = await fetch('/api/presets');
	return res.json();
}

export async function resummarize(sessionId: string, preset?: string): Promise<void> {
	await fetch(`/api/sessions/${sessionId}/resummarize`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(preset ? { preset } : {}),
	});
}
```

**Step 3: Add presets to app state**

In `state.svelte.ts`, add `presets` to `AppState` and a `setPresets()` function. Load presets on app init (alongside the status poll).

**Step 4: Add resummarize UI to session card**

On session cards with `summary_status === 'completed'` or `'failed'`:
- Small "Resummarize" button
- Clicking shows a dropdown of preset names (from state)
- Selecting a preset calls `resummarize(sessionId, presetName)`
- Session card transitions to "Summarizing..." state

**Step 5: Run frontend build + lint + test**

Run: `npm run build && npm run lint && npm test` (from `web/` dir)
Expected: PASS

**Step 6: Commit**

Message: `feat(web): add resummarize UI with preset selection`

---

### Task 11: Wire Everything in main.go

**Files:**
- Modify: `cmd/ghost-wispr/main.go`
- Modify: `ghost-wispr.yaml.example`
- Modify: `.env.example`

**Step 1: Update main.go**

Replace the OpenAI-specific summarizer construction:

```go
// Old:
// var summarizer session.Summarizer
// if cfg.OpenAIAPIKey != "" {
//     summarizer = summary.NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIModel, store)
// }

// New:
apiKeys := map[string]string{
	"openai":    cfg.OpenAIAPIKey,
	"anthropic": cfg.AnthropicAPIKey,
	"gemini":    cfg.GeminiAPIKey,
}

clientFactory := func(provider, model string) (llm.Client, error) {
	key := apiKeys[provider]
	if key == "" {
		return nil, fmt.Errorf("no API key for provider %q", provider)
	}
	var opts []llm.Option
	if cfg.Summarization.BaseURL != "" {
		opts = append(opts, llm.WithBaseURL(cfg.Summarization.BaseURL))
	}
	return llm.NewClient(provider, key, model, opts...)
}

var summarizer *summary.Summarizer
if provider, _, err := llm.ParseModel(cfg.Summarization.Model); err == nil && apiKeys[provider] != "" {
	summarizer = summary.New(cfg.Summarization, clientFactory, store)
}
```

Wire the `Resummarize` hook into `ControlHooks`:
```go
Presets: func() map[string]config.Preset {
	if summarizer == nil {
		return nil
	}
	return summarizer.Presets()
},
Resummarize: func(ctx context.Context, sessionID, preset string) error {
	// Load segments, build transcript, call summarizer
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, seg := range segments {
		b.WriteString(seg.Text)
		b.WriteString("\n")
	}
	transcript := b.String()

	var summaryText string
	var presetUsed string
	if preset != "" {
		presetUsed = preset
		summaryText, err = summarizer.SummarizeWithPreset(ctx, sessionID, transcript, preset)
	} else {
		// Let router pick
		summaryText, err = summarizer.Summarize(ctx, sessionID, transcript)
		presetUsed = "default" // TODO: get actual preset from summarizer
	}

	status := storage.SummaryCompleted
	if err != nil {
		status = storage.SummaryFailed
	}
	_ = store.UpdateSummary(sessionID, summaryText, status, presetUsed)
	if hub != nil {
		hub.BroadcastSummaryReady(sessionID, summaryText, status)
	}
	return err
},
```

**Step 2: Update manager.go**

Update `generateSummary` to pass preset name to `UpdateSummary`. Since the current `session.Summarizer` interface returns `(string, error)`, and we need the preset name too, we have two options:

Option A: Change `Summarizer` interface to return `(summary, preset string, err error)`.
Option B: Have `manager.go` always pass `""` for auto-summarization preset (it doesn't know which was picked), and only resummarize (via API) stores the preset.

Go with Option A for correctness — the `Summarizer.Summarize` interface becomes:
```go
type Summarizer interface {
	Summarize(ctx context.Context, sessionID, transcript string) (summary, preset string, err error)
}
```

Update `manager.go` and `manager_test.go` accordingly.

**Step 3: Update example files**

`ghost-wispr.yaml.example`:
```yaml
summarization:
  model: openai/gpt-4o-mini
  # base_url: ""  # Optional: for OpenAI-compatible endpoints

  presets:
    default:
      description: "General-purpose meeting summary with key topics, decisions, and action items"
      system_prompt: "Summarize the following office conversation transcript concisely in markdown. Include key topics, decisions made, and action items if any."
      user_template: "{{transcript}}"
```

`.env.example`:
```
GHOST_WISPR_DEEPGRAM_API_KEY=
GHOST_WISPR_OPENAI_API_KEY=
GHOST_WISPR_ANTHROPIC_API_KEY=
GHOST_WISPR_GEMINI_API_KEY=
```

**Step 4: Run full test suite**

Run: `go test ./... -count=1`
Run: `golangci-lint run ./...`
Run: `cd web && npm run build && npm run lint && npm test`
Expected: All pass

**Step 5: Commit**

Message: `feat: wire provider-agnostic summarization into main`

---

### Task 12: Final Cleanup + Verification

**Files:**
- Verify all old OpenAI-specific references are removed
- Move design doc: copy `.sisyphus/drafts/provider-agnostic-summarization.md` → `docs/plans/2026-02-27-provider-agnostic-summarization-design.md`

**Step 1: Search for stale references**

Run: `grep -r "OpenAIModel\|OPENAI_MODEL\|openai_model" --include="*.go" --include="*.yaml" --include="*.ts"`
Expected: No results (all removed)

**Step 2: Run full verification**

Run: `go test ./... -count=1 && golangci-lint run ./...`
Run: `cd web && npm run build && npm run lint && npm test`
Expected: All green

**Step 3: Final commit**

Message: `chore: remove stale OpenAI-specific config references`

---

Plan complete and saved to `docs/plans/2026-02-27-provider-agnostic-summarization.md`.

Two execution options:

**1. Subagent-Driven (this session)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Parallel Session (separate)** — Open a new session with `executing-plans`, batch execution with checkpoints.

Which approach?