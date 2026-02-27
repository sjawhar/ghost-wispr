package summary

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

func TestRouterSelectsCorrectPreset(t *testing.T) {
	client := &mockLLMClient{response: "engineering"}
	cfg := config.Summarization{
		Model: "openai/gpt-4o-mini",
		Presets: map[string]config.Preset{
			"default": {
				Description: "general",
			},
			"engineering": {
				Description: "technical",
			},
		},
	}

	router := NewRouter(cfg, func(provider, model string) (llm.Client, error) {
		if provider != "openai" {
			t.Fatalf("expected provider openai, got %q", provider)
		}
		if model != "gpt-4o-mini" {
			t.Fatalf("expected model gpt-4o-mini, got %q", model)
		}
		return client, nil
	})

	preset, err := router.SelectPreset(context.Background(), buildNumberedTranscript(900))
	if err != nil {
		t.Fatalf("SelectPreset failed: %v", err)
	}
	if preset != "engineering" {
		t.Fatalf("expected engineering preset, got %q", preset)
	}
	if client.calls != 1 {
		t.Fatalf("expected one llm call, got %d", client.calls)
	}
}

func TestRouterFallsBackToDefault(t *testing.T) {
	client := &mockLLMClient{response: "gibberish-output"}
	cfg := config.Summarization{
		Model: "openai/gpt-4o-mini",
		Presets: map[string]config.Preset{
			"default": {
				Description: "general",
			},
			"sales": {
				Description: "sales",
			},
		},
	}

	router := NewRouter(cfg, func(_, _ string) (llm.Client, error) {
		return client, nil
	})

	preset, err := router.SelectPreset(context.Background(), buildNumberedTranscript(900))
	if err != nil {
		t.Fatalf("SelectPreset failed: %v", err)
	}
	if preset != "default" {
		t.Fatalf("expected default preset fallback, got %q", preset)
	}
}

func TestSampleTranscript(t *testing.T) {
	transcript := buildNumberedTranscript(1000)
	sampled := SampleTranscript(transcript, 300, 200, 200)

	if strings.Count(sampled, "[...]") != 2 {
		t.Fatalf("expected two omission markers, got %q", sampled)
	}
	if !strings.Contains(sampled, "w1 w2") {
		t.Fatalf("expected first chunk to start from beginning, got %q", sampled)
	}
	if !strings.Contains(sampled, "w300") {
		t.Fatalf("expected first chunk to include w300, got %q", sampled)
	}
	if !strings.Contains(sampled, "w401") || !strings.Contains(sampled, "w600") {
		t.Fatalf("expected middle chunk around w401-w600, got %q", sampled)
	}
	if !strings.Contains(sampled, "w801") || !strings.HasSuffix(sampled, "w1000") {
		t.Fatalf("expected last chunk ending in w1000, got %q", sampled)
	}
}

func TestSampleTranscriptShortText(t *testing.T) {
	transcript := buildNumberedTranscript(10)
	if got := SampleTranscript(transcript, 5, 3, 3); got != transcript {
		t.Fatalf("expected full transcript, got %q", got)
	}
}

func TestRouter_FallbackUsesFirstPreset(t *testing.T) {
	// No "default" preset — fallback should return first sorted key ("brief")
	cfg := config.Summarization{
		Model: "not/valid", // invalid model forces ParseModel error → fallback path
		Presets: map[string]config.Preset{
			"detailed": {Description: "detailed notes"},
			"brief":    {Description: "quick summary"},
		},
	}

	router := NewRouter(cfg, func(_, _ string) (llm.Client, error) {
		return nil, fmt.Errorf("should not be called")
	})

	preset, err := router.SelectPreset(context.Background(), buildNumberedTranscript(900))
	if err != nil {
		t.Fatalf("SelectPreset failed: %v", err)
	}
	if preset != "brief" {
		t.Fatalf("expected first sorted preset 'brief', got %q", preset)
	}
}

func buildNumberedTranscript(wordCount int) string {
	words := make([]string, 0, wordCount)
	for i := 1; i <= wordCount; i++ {
		words = append(words, fmt.Sprintf("w%d", i))
	}
	return strings.Join(words, " ")
}
