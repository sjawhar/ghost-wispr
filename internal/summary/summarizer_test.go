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
	lastMessages []llm.Message
}

func (m *mockLLMClient) Complete(_ context.Context, messages []llm.Message) (string, error) {
	m.calls++
	m.lastMessages = append([]llm.Message(nil), messages...)
	if m.err != nil && m.calls <= m.failUntil {
		return "", m.err
	}
	return m.response, nil
}

func (m *mockLLMClient) CompleteJSON(_ context.Context, messages []llm.Message, _ map[string]any) (json.RawMessage, error) {
	m.calls++
	m.lastMessages = append([]llm.Message(nil), messages...)
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

	title, summaryText, preset, err := s.Summarize(context.Background(), "session-1", transcript)
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

	title, summaryText, preset, err := s.Summarize(context.Background(), "session-1", "too short")
	if err != nil {
		t.Fatalf("Summarize returned error: %v", err)
	}
	if title != "" {
		t.Fatalf("expected empty title, got %q", title)
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

	_, _, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
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

	title, summaryText, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "detailed")
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
	client := &mockLLMClient{response: `{"title":"Retry title","summary":"retry-success"}`, err: errors.New("temporary"), failUntil: 4}
	var sleeps []time.Duration

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
	s.sleep = func(d time.Duration) {
		sleeps = append(sleeps, d)
	}

	title, summaryText, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
	}
	if title != "Retry title" {
		t.Fatalf("expected Retry title, got %q", title)
	}
	if summaryText != "retry-success" {
		t.Fatalf("expected retry-success, got %q", summaryText)
	}
	// Flow: CompleteJSON fail (1), Complete fallback fail (2), retry CompleteJSON fail (3), sleep,
	// retry CompleteJSON fail (4), sleep, retry CompleteJSON succeed (5).
	if client.calls != 5 {
		t.Fatalf("expected 5 llm calls, got %d", client.calls)
	}
	if len(sleeps) != 2 {
		t.Fatalf("expected 2 sleep calls, got %d", len(sleeps))
	}
	if sleeps[0] != time.Second || sleeps[1] != 4*time.Second {
		t.Fatalf("unexpected sleep durations: %#v", sleeps)
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

	_, _, err := s.SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "missing")
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
