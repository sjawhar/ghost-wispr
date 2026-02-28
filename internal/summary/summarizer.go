package summary

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

type ClientFactory func(provider, model string) (llm.Client, error)

type Summarizer struct {
	cfg     config.Summarization
	factory ClientFactory
	router  *Router
	sleep   func(time.Duration)
}

func New(cfg config.Summarization, factory ClientFactory) *Summarizer {
	var router *Router
	if len(cfg.Presets) > 1 {
		router = NewRouter(cfg, factory)
	}
	return &Summarizer{
		cfg:     cfg,
		factory: factory,
		router:  router,
		sleep:   time.Sleep,
	}
}

func (s *Summarizer) Summarize(ctx context.Context, sessionID, transcript string) (string, string, error) {
	presetName, err := s.selectPreset(ctx, transcript)
	if err != nil {
		return "", "", fmt.Errorf("select preset: %w", err)
	}
	summary, err := s.SummarizeWithPreset(ctx, sessionID, transcript, presetName)
	return summary, presetName, err
}

func (s *Summarizer) SummarizeWithPreset(ctx context.Context, _ string, transcript, presetName string) (string, error) {
	if len(strings.Fields(transcript)) < 20 {
		return "", nil
	}

	preset, ok := s.cfg.Presets[presetName]
	if !ok {
		return "", fmt.Errorf("unknown preset %q", presetName)
	}

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

	date := time.Now().UTC().Format("2006-01-02")
	userContent := strings.ReplaceAll(preset.UserTemplate, "{{transcript}}", transcript)
	userContent = strings.ReplaceAll(userContent, "{{date}}", date)

	messages := []llm.Message{
		{Role: "system", Content: preset.SystemPrompt},
		{Role: "user", Content: userContent},
	}

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
		for name := range s.cfg.Presets {
			return name, nil
		}
		return "default", nil
	}
	return s.router.SelectPreset(ctx, transcript)
}

func (s *Summarizer) Presets() map[string]config.Preset {
	return s.cfg.Presets
}
