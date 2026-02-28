package summary

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
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
		slog.Warn("router: falling back to default preset", "reason", "parse model failed", "error", err)
		return r.fallbackPreset(), nil
	}

	client, err := r.factory(provider, model)
	if err != nil {
		slog.Warn("router: falling back to default preset", "reason", "create client failed", "error", err)
		return r.fallbackPreset(), nil
	}

	result, err := client.Complete(ctx, []llm.Message{{Role: "user", Content: prompt}})
	if err != nil {
		slog.Warn("router: falling back to default preset", "reason", "llm complete failed", "error", err)
		return r.fallbackPreset(), nil
	}

	chosen := strings.TrimSpace(result)
	if _, ok := r.cfg.Presets[chosen]; ok {
		return chosen, nil
	}

	slog.Warn("router: falling back to default preset", "reason", "chosen preset not found", "chosen", chosen)
	return r.fallbackPreset(), nil
}

func (r *Router) fallbackPreset() string {
	if _, ok := r.cfg.Presets["default"]; ok {
		return "default"
	}
	keys := make([]string, 0, len(r.cfg.Presets))
	for k := range r.cfg.Presets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0]
}
