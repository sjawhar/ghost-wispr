package summary

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

type mockLLMClient struct {
	calls        int
	response     string
	err          error
	lastMessages []llm.Message
}

func (m *mockLLMClient) Complete(_ context.Context, messages []llm.Message) (string, error) {
	m.calls++
	m.lastMessages = append([]llm.Message(nil), messages...)
	if m.err != nil && m.calls < 3 {
		return "", m.err
	}
	return m.response, nil
}

func TestSummarizeSinglePreset(t *testing.T) {
	transcript := buildTranscript(25)
	client := &mockLLMClient{response: "## Summary"}
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

	summaryText, preset, err := s.Summarize(context.Background(), "session-1", transcript)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
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
	client := &mockLLMClient{response: "should-not-be-used"}

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

	summaryText, preset, err := s.Summarize(context.Background(), "session-1", "too short")
	if err != nil {
		t.Fatalf("Summarize returned error: %v", err)
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
	client := &mockLLMClient{response: "ok"}

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

	_, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
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
	client := &mockLLMClient{response: "preset-summary"}

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

	summaryText, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "detailed")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
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
	client := &mockLLMClient{response: "retry-success", err: errors.New("temporary")}
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

	summaryText, err := s.SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset failed: %v", err)
	}
	if summaryText != "retry-success" {
		t.Fatalf("expected retry-success, got %q", summaryText)
	}
	if client.calls != 3 {
		t.Fatalf("expected 3 llm calls, got %d", client.calls)
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

	_, err := s.SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "missing")
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
