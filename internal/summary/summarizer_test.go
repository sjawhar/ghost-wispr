package summary

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

type mockLLMClient struct {
	calls        int
	failUntil    int // fail until calls reaches this number (default: 0 = never fail)
	response     string
	err          error
	jsonQueue    []mockJSONResult
	textQueue    []mockTextResult
	lastMessages []llm.Message
	lastSchema   map[string]any
}

type mockJSONResult struct {
	response string
	err      error
}

type mockTextResult struct {
	response string
	err      error
}

func (m *mockLLMClient) Complete(_ context.Context, messages []llm.Message) (string, error) {
	m.calls++
	m.lastMessages = append([]llm.Message(nil), messages...)
	if len(m.textQueue) > 0 {
		item := m.textQueue[0]
		m.textQueue = m.textQueue[1:]
		if item.err != nil {
			return "", item.err
		}
		return item.response, nil
	}
	if m.err != nil && m.calls <= m.failUntil {
		return "", m.err
	}
	return m.response, nil
}

func (m *mockLLMClient) CompleteJSON(_ context.Context, messages []llm.Message, schema map[string]any) (json.RawMessage, error) {
	m.calls++
	m.lastMessages = append([]llm.Message(nil), messages...)
	m.lastSchema = schema
	if len(m.jsonQueue) > 0 {
		item := m.jsonQueue[0]
		m.jsonQueue = m.jsonQueue[1:]
		if item.err != nil {
			return nil, item.err
		}
		return json.RawMessage(item.response), nil
	}
	if m.err != nil && m.calls <= m.failUntil {
		return nil, m.err
	}
	return json.RawMessage(m.response), nil
}

func TestSummarizeSinglePreset(t *testing.T) {
	transcript := buildTranscript(25)
	client := &mockLLMClient{response: `{"title":"Auto title","summary":"## Summary"}`}
	factoryCalls := 0

	cfg := config.Summarization{
		Model: "openai/gpt-4o-mini",
		Presets: map[string]config.Preset{
			"default": {
				Description:  "general",
				SystemPrompt: "system",
				UserTemplate: "{{transcript}}",
			},
		},
	}

	s := New(cfg, func(provider, model string) (llm.Client, error) {
		if provider != "openai" {
			t.Fatalf("expected provider openai, got %q", provider)
		}
		if model != "gpt-4o-mini" {
			t.Fatalf("expected model gpt-4o-mini, got %q", model)
		}
		factoryCalls++
		return client, nil
	})
	s.sleep = func(time.Duration) {}

	title, summaryText, preset, _, err := s.Summarize(context.Background(), "session-1", transcript)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}
	if title != "Auto title" {
		t.Fatalf("expected title Auto title, got %q", title)
	}
	if summaryText != "## Summary" {
		t.Fatalf("expected summary ## Summary, got %q", summaryText)
	}
	if preset != "default" {
		t.Fatalf("expected preset default, got %q", preset)
	}
	if client.calls != 1 {
		t.Fatalf("expected 1 llm call, got %d", client.calls)
	}
	if factoryCalls != 1 {
		t.Fatalf("expected 1 factory call, got %d", factoryCalls)
	}
}

func TestSummarizeSkipsShortTranscript(t *testing.T) {
	client := &mockLLMClient{response: `{"title":"unused","summary":"should-not-be-used"}`}

	cfg := config.Summarization{
		Model: "openai/gpt-4o-mini",
		Presets: map[string]config.Preset{
			"default": {
				Description:  "general",
				SystemPrompt: "system",
				UserTemplate: "{{transcript}}",
			},
		},
	}

	s := New(cfg, func(_, _ string) (llm.Client, error) {
		return client, nil
	})

	title, summaryText, preset, _, err := s.Summarize(context.Background(), "session-1", "too short")
	if err != nil {
		t.Fatalf("Summarize returned error: %v", err)
	}
	if title == "" {
		t.Fatal("expected non-empty fallback title")
	}
	if summaryText != "" {
		t.Fatalf("expected empty summary, got %q", summaryText)
	}
	if preset != "default" {
		t.Fatalf("expected default preset, got %q", preset)
	}
	if client.calls != 0 {
		t.Fatalf("expected zero llm calls, got %d", client.calls)
	}
}

func TestSummarizeRendersTemplate(t *testing.T) {
	transcript := buildTranscript(25)
	client := &mockLLMClient{response: `{"title":"Title","summary":"ok"}`}

	cfg := config.Summarization{
		Model: "openai/gpt-4o-mini",
		Presets: map[string]config.Preset{
			"default": {
				Description:  "general",
				SystemPrompt: "system",
				UserTemplate: "Date={{date}}\nBody={{transcript}}",
			},
		},
	}

	s := New(cfg, func(_, _ string) (llm.Client, error) {
		return client, nil
	})

	_, _, _, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
	}

	if len(client.lastMessages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(client.lastMessages))
	}
	today := time.Now().UTC().Format("2006-01-02")
	if !strings.Contains(client.lastMessages[1].Content, "Date="+today) {
		t.Fatalf("expected rendered date in user content, got %q", client.lastMessages[1].Content)
	}
	if !strings.Contains(client.lastMessages[1].Content, "Body="+transcript) {
		t.Fatalf("expected rendered transcript in user content, got %q", client.lastMessages[1].Content)
	}
}

func TestSummarizeWithPreset(t *testing.T) {
	transcript := buildTranscript(25)
	client := &mockLLMClient{response: `{"title":"Preset title","summary":"preset-summary"}`}

	cfg := config.Summarization{
		Model: "not/a-valid/global-model",
		Presets: map[string]config.Preset{
			"default": {
				Description:  "general",
				SystemPrompt: "system",
				UserTemplate: "{{transcript}}",
				Model:        "openai/gpt-4o-mini",
			},
			"detailed": {
				Description:  "detailed",
				SystemPrompt: "system",
				UserTemplate: "{{transcript}}",
				Model:        "openai/gpt-4o-mini",
			},
		},
	}

	s := New(cfg, func(_, _ string) (llm.Client, error) {
		return client, nil
	})

	title, summaryText, _, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "detailed")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
	}
	if title != "Preset title" {
		t.Fatalf("expected Preset title, got %q", title)
	}
	if summaryText != "preset-summary" {
		t.Fatalf("expected preset-summary, got %q", summaryText)
	}
	if client.calls != 1 {
		t.Fatalf("expected one llm call, got %d", client.calls)
	}
}

func TestSummarizeRetries(t *testing.T) {
	transcript := buildTranscript(25)
	client := &mockLLMClient{jsonQueue: []mockJSONResult{
		{err: errors.New("openai json completion: status 429 rate limit")},
		{err: errors.New("openai json completion: status 429 rate limit")},
		{err: errors.New("openai json completion: status 429 rate limit")},
		{err: errors.New("openai json completion: status 429 rate limit")},
		{response: `{"title":"Retry title","summary":"retry-success"}`},
	}}

	cfg := config.Summarization{
		Model: "openai/gpt-4o-mini",
		Presets: map[string]config.Preset{
			"default": {
				Description:  "general",
				SystemPrompt: "system",
				UserTemplate: "{{transcript}}",
			},
		},
	}

	s := New(cfg, func(_, _ string) (llm.Client, error) {
		return client, nil
	})

	title, summaryText, _, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
	}
	if title != "Retry title" {
		t.Fatalf("expected Retry title, got %q", title)
	}
	if summaryText != "retry-success" {
		t.Fatalf("expected retry-success, got %q", summaryText)
	}
	if client.calls != 5 {
		t.Fatalf("expected 5 llm calls, got %d", client.calls)
	}
}

func TestRetryOnRateLimit429(t *testing.T) {
	transcript := buildTranscript(25)
	client := &mockLLMClient{jsonQueue: []mockJSONResult{
		{err: errors.New("openai json completion: status 429 rate_limit")},
		{err: errors.New("openai json completion: status 429 rate_limit")},
		{response: `{"title":"Recovered","summary":"done"}`},
	}}

	cfg := config.Summarization{Model: "openai/gpt-4o-mini", Presets: map[string]config.Preset{"default": {
		Description: "general", SystemPrompt: "system", UserTemplate: "{{transcript}}",
	}}}

	s := New(cfg, func(_, _ string) (llm.Client, error) { return client, nil })

	title, summaryText, _, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
	}
	if title != "Recovered" || summaryText != "done" {
		t.Fatalf("unexpected output title=%q summary=%q", title, summaryText)
	}
	if client.calls != 3 {
		t.Fatalf("expected 3 llm calls, got %d", client.calls)
	}
}

func TestNoRetryOnBadRequest400(t *testing.T) {
	transcript := buildTranscript(25)
	client := &mockLLMClient{jsonQueue: []mockJSONResult{{err: errors.New("openai json completion: status 400 invalid_schema")}}}

	cfg := config.Summarization{Model: "openai/gpt-4o-mini", Presets: map[string]config.Preset{"default": {
		Description: "general", SystemPrompt: "system", UserTemplate: "{{transcript}}",
	}}}

	s := New(cfg, func(_, _ string) (llm.Client, error) { return client, nil })

	_, _, _, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err == nil {
		t.Fatal("expected error")
	}
	if client.calls != 1 {
		t.Fatalf("expected single attempt on 400, got %d", client.calls)
	}
}

func TestChunkingLongTranscriptAndMerge(t *testing.T) {
	transcript := buildTranscript(120)
	client := &mockLLMClient{
		textQueue: []mockTextResult{{response: "chunk one summary"}, {response: "chunk two summary"}},
		jsonQueue: []mockJSONResult{{response: `{"title":"Merged Title","summary":"Merged BLUF summary"}`}},
	}

	cfg := config.Summarization{Model: "openai/gpt-4o-mini", Presets: map[string]config.Preset{"default": {
		Description: "general", SystemPrompt: "system", UserTemplate: "{{transcript}}",
	}}}

	s := New(cfg, func(_, _ string) (llm.Client, error) { return client, nil })
	s.chunkTokenThreshold = 20
	s.chunkSizeTokens = 80
	s.chunkOverlapTokens = 20

	title, summaryText, _, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
	}
	if title != "Merged Title" {
		t.Fatalf("expected merged title, got %q", title)
	}
	if summaryText != "Merged BLUF summary" {
		t.Fatalf("expected merged summary, got %q", summaryText)
	}
	if client.calls != 3 {
		t.Fatalf("expected 3 calls (2 chunk + 1 merge), got %d", client.calls)
	}
}

func TestPromptAndSchemaStructuredConstraints(t *testing.T) {
	transcript := buildTranscript(25)
	client := &mockLLMClient{response: `{"title":"Prompt title","summary":"Prompt summary"}`}

	cfg := config.Summarization{Model: "openai/gpt-4o-mini", Presets: map[string]config.Preset{"default": {
		Description: "general", SystemPrompt: "legacy system", UserTemplate: "{{transcript}}",
	}}}

	s := New(cfg, func(_, _ string) (llm.Client, error) { return client, nil })

	_, _, _, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
	}
	if len(client.lastMessages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(client.lastMessages))
	}
	if !strings.Contains(strings.ToLower(client.lastMessages[0].Content), "strategic meeting analyst") {
		t.Fatalf("expected structured system prompt, got %q", client.lastMessages[0].Content)
	}
	if !strings.Contains(client.lastMessages[1].Content, "BLUF") {
		t.Fatalf("expected BLUF instruction in user prompt, got %q", client.lastMessages[1].Content)
	}
	if client.lastSchema == nil {
		t.Fatal("expected JSON schema to be passed")
	}
	props, ok := client.lastSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties object in schema, got %#v", client.lastSchema)
	}
	titleProp, ok := props["title"].(map[string]any)
	if !ok {
		t.Fatalf("expected title schema, got %#v", props["title"])
	}
	if _, ok := titleProp["description"]; !ok {
		t.Fatalf("expected title description in schema, got %#v", titleProp)
	}
}

func TestSpeakerNameExtraction(t *testing.T) {
	t.Run("mentioned speaker name extracted", func(t *testing.T) {
		transcript := strings.Join([]string{
			"Speaker 0: Let's review the launch checklist.",
			"Speaker 1: Hey Ben, what do you think?",
			"Speaker 0: We'll ship Friday if QA passes.",
		}, " ")

		client := &mockLLMClient{response: `{"title":"Launch readiness","summary":"## BLUF\nReady to ship Friday pending QA.","speakers":{"1":{"name":"Ben","confidence":"mentioned"}}}`}
		cfg := config.Summarization{Model: "openai/gpt-4o-mini", Presets: map[string]config.Preset{"default": {
			Description: "general", SystemPrompt: "system", UserTemplate: "{{transcript}}",
		}}}

		s := New(cfg, func(_, _ string) (llm.Client, error) { return client, nil })

		_, _, _, speakerNames, err := s.Summarize(context.Background(), "session-1", transcript)
		if err != nil {
			t.Fatalf("Summarize failed: %v", err)
		}

		expected := `{"1":{"name":"Ben","confidence":"mentioned"}}`
		if speakerNames != expected {
			t.Fatalf("expected speaker names %s, got %s", expected, speakerNames)
		}
	})

	t.Run("no names stores empty json", func(t *testing.T) {
		transcript := buildTranscript(25)
		client := &mockLLMClient{response: `{"title":"General sync","summary":"## BLUF\nNo names mentioned.","speakers":{}}`}
		cfg := config.Summarization{Model: "openai/gpt-4o-mini", Presets: map[string]config.Preset{"default": {
			Description: "general", SystemPrompt: "system", UserTemplate: "{{transcript}}",
		}}}

		s := New(cfg, func(_, _ string) (llm.Client, error) { return client, nil })

		_, _, _, speakerNames, err := s.Summarize(context.Background(), "session-1", transcript)
		if err != nil {
			t.Fatalf("Summarize failed: %v", err)
		}
		if speakerNames != "{}" {
			t.Fatalf("expected empty speaker names JSON {}, got %s", speakerNames)
		}
	})
}

func TestContextOverflowFallsBackToChunking(t *testing.T) {
	transcript := buildTranscript(60)
	client := &mockLLMClient{
		jsonQueue: []mockJSONResult{
			{err: errors.New("openai json completion: status 400 context length exceeded")},
			{response: `{"title":"Chunked Title","summary":"final"}`},
		},
		textQueue: []mockTextResult{{response: "chunk a"}, {response: "chunk b"}},
	}

	cfg := config.Summarization{Model: "openai/gpt-4o-mini", Presets: map[string]config.Preset{"default": {
		Description: "general", SystemPrompt: "system", UserTemplate: "{{transcript}}",
	}}}

	s := New(cfg, func(_, _ string) (llm.Client, error) { return client, nil })
	s.chunkTokenThreshold = 1000
	s.chunkSizeTokens = 40
	s.chunkOverlapTokens = 20

	title, _, _, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
	}
	if title != "Chunked Title" {
		t.Fatalf("expected chunked title, got %q", title)
	}
	if client.calls != 4 {
		t.Fatalf("expected 4 calls (1 failed json + 2 chunks + 1 merge), got %d", client.calls)
	}
}

func TestSummarizeUnknownPreset(t *testing.T) {
	cfg := config.Summarization{
		Model: "openai/gpt-4o-mini",
		Presets: map[string]config.Preset{
			"default": {
				Description:  "general",
				SystemPrompt: "system",
				UserTemplate: "{{transcript}}",
			},
		},
	}

	s := New(cfg, func(_, _ string) (llm.Client, error) {
		return &mockLLMClient{response: "ok"}, nil
	})

	_, _, _, err := s.SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "missing")
	if err == nil {
		t.Fatal("expected unknown preset error")
	}
	if !strings.Contains(err.Error(), "unknown preset") {
		t.Fatalf("expected unknown preset error, got %v", err)
	}
}

func buildTranscript(wordCount int) string {
	words := make([]string, 0, wordCount)
	for i := 0; i < wordCount; i++ {
		words = append(words, "word")
	}
	return strings.Join(words, " ")
}
