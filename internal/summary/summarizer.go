package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cenkalti/backoff/v5"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/retry"
)

type ClientFactory func(provider, model string) (llm.Client, error)

type Summarizer struct {
	cfg                 config.Summarization
	factory             ClientFactory
	router              *Router
	sleep               func(time.Duration)
	now                 func() time.Time
	chunkTokenThreshold int
	chunkSizeTokens     int
	chunkOverlapTokens  int
}

const (
	minSummaryWords      = 20
	tokenPerCharEstimate = 4
)

func New(cfg config.Summarization, factory ClientFactory) *Summarizer {
	var router *Router
	if len(cfg.Presets) > 1 {
		router = NewRouter(cfg, factory)
	}
	return &Summarizer{
		cfg:                 cfg,
		factory:             factory,
		router:              router,
		sleep:               time.Sleep,
		now:                 time.Now,
		chunkTokenThreshold: 100_000,
		chunkSizeTokens:     10_000,
		chunkOverlapTokens:  500,
	}
}

func (s *Summarizer) Summarize(ctx context.Context, sessionID, transcript string) (string, string, string, string, error) {
	presetName, err := s.selectPreset(ctx, transcript)
	if err != nil {
		return "", "", "", "{}", fmt.Errorf("select preset: %w", err)
	}
	title, summary, speakerNames, err := s.SummarizeWithPreset(ctx, sessionID, transcript, presetName)
	return title, summary, presetName, speakerNames, err
}

func (s *Summarizer) SummarizeWithPreset(ctx context.Context, _ string, transcript, presetName string) (string, string, string, error) {
	if len(strings.Fields(transcript)) < minSummaryWords {
		return guaranteedTitle(transcript), "", "{}", nil
	}

	preset, ok := s.cfg.Presets[presetName]
	if !ok {
		return "", "", "{}", fmt.Errorf("unknown preset %q", presetName)
	}

	modelStr := preset.Model
	if modelStr == "" {
		modelStr = s.cfg.Model
	}

	provider, model, err := llm.ParseModel(modelStr)
	if err != nil {
		return "", "", "{}", err
	}

	client, err := s.factory(provider, model)
	if err != nil {
		return "", "", "{}", fmt.Errorf("create llm client: %w", err)
	}

	logger := logging.WithModule(logging.FromContext(ctx, nil), "summary")
	if s.shouldChunk(transcript) {
		title, summary, speakerNames, err := s.summarizeChunked(ctx, client, preset, transcript, logger)
		if err != nil {
			return guaranteedTitle(transcript), "", "{}", err
		}
		if strings.TrimSpace(title) == "" {
			title = guaranteedTitle(transcript)
		}
		return title, strings.TrimSpace(summary), speakerNames, nil
	}

	messages := s.structuredMessages(preset, transcript)
	schema := structuredSchema()
	raw, err := s.completeJSONWithRetry(ctx, client, messages, schema, logger)
	if err != nil {
		if isContextOverflow(err) {
			title, summary, speakerNames, chunkErr := s.summarizeChunked(ctx, client, preset, transcript, logger)
			if chunkErr == nil {
				if strings.TrimSpace(title) == "" {
					title = guaranteedTitle(transcript)
				}
				return title, strings.TrimSpace(summary), speakerNames, nil
			}
			return guaranteedTitle(transcript), "", "{}", fmt.Errorf("summarize chunked after overflow: %w", chunkErr)
		}
		return guaranteedTitle(transcript), "", "{}", err
	}

	parsed, parseErr := parseStructuredSummary(raw)
	if parseErr != nil {
		return guaranteedTitle(transcript), strings.TrimSpace(string(raw)), "{}", nil
	}
	if strings.TrimSpace(parsed.Title) == "" {
		parsed.Title = guaranteedTitle(transcript)
	}

	return strings.TrimSpace(parsed.Title), strings.TrimSpace(parsed.Summary), parsed.speakerNamesJSON(), nil
}

type parsedSummary struct {
	Title    string                         `json:"title"`
	Summary  string                         `json:"summary"`
	Speakers map[string]speakerNameMetadata `json:"speakers,omitempty"`
}

type speakerNameMetadata struct {
	Name       string `json:"name"`
	Confidence string `json:"confidence"`
}

func parseStructuredSummary(raw json.RawMessage) (parsedSummary, error) {
	var parsed parsedSummary
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return parsedSummary{}, err
	}
	for speakerID, meta := range parsed.Speakers {
		meta.Name = strings.TrimSpace(meta.Name)
		meta.Confidence = strings.TrimSpace(meta.Confidence)
		if meta.Name == "" {
			delete(parsed.Speakers, speakerID)
			continue
		}
		if meta.Confidence != "mentioned" && meta.Confidence != "inferred" {
			meta.Confidence = "inferred"
		}
		parsed.Speakers[speakerID] = meta
	}
	return parsed, nil
}

func (p parsedSummary) speakerNamesJSON() string {
	if len(p.Speakers) == 0 {
		return "{}"
	}
	b, err := json.Marshal(p.Speakers)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (s *Summarizer) shouldChunk(transcript string) bool {
	tokenEstimate := utf8.RuneCountInString(transcript) / tokenPerCharEstimate
	return tokenEstimate > s.chunkTokenThreshold
}

func (s *Summarizer) structuredMessages(preset config.Preset, transcript string) []llm.Message {
	rendered := strings.ReplaceAll(preset.UserTemplate, "{{transcript}}", transcript)
	rendered = strings.ReplaceAll(rendered, "{{date}}", s.now().UTC().Format("2006-01-02"))

	system := strings.TrimSpace(strings.Join([]string{
		"You are a strategic meeting analyst.",
		"Return only valid JSON matching the provided schema.",
		"The title must be non-empty and concise.",
		strings.TrimSpace(preset.SystemPrompt),
	}, "\n"))

	user := strings.TrimSpace(strings.Join([]string{
		"Summarize the transcript in BLUF style.",
		"Also extract speaker names if mentioned in the transcript. Use 'mentioned' confidence if the name was explicitly said, 'inferred' if you're guessing from context.",
		"Output requirements:",
		`- title: 4-10 words, specific, non-empty`,
		`- summary: markdown starting with '## BLUF', followed by sections for Decisions, Key Outcomes, and Risks/Notes`,
		`- speakers: object keyed by speaker ID (for example "0", "1"), each value with fields {"name":"<speaker name>","confidence":"mentioned|inferred"}`,
		"Respond as JSON only.",
		"Example output:",
		`{"title":"Q2 roadmap alignment","summary":"## BLUF\nTeam aligned on Q2 roadmap and sequencing.\n\n## Decisions\n- Ship feature flags before rollout.\n\n## Key Outcomes\n- Roadmap finalized and communicated.\n\n## Risks/Notes\n- API dependency may delay launch by one sprint.","speakers":{"0":{"name":"Ben","confidence":"mentioned"}}}`,
		"Transcript:",
		rendered,
	}, "\n\n"))

	return []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
}

func structuredSchema() map[string]any {
	required := []string{"summary", "title"}
	sort.Strings(required)
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "Non-empty meeting title in 4-10 words.",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "Markdown BLUF summary with Decisions, Key Outcomes, and Risks/Notes sections.",
			},
			"speakers": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
						"confidence": map[string]any{
							"type": "string",
							"enum": []string{"mentioned", "inferred"},
						},
					},
				},
			},
		},
		"required":             required,
		"additionalProperties": false,
	}
}

func (s *Summarizer) completeJSONWithRetry(ctx context.Context, client llm.Client, messages []llm.Message, schema map[string]any, logger *slog.Logger) (json.RawMessage, error) {
	var result json.RawMessage
	err := retry.Do(ctx, func() error {
		var callErr error
		result, callErr = client.CompleteJSON(ctx, messages, schema)
		if callErr != nil && isContextOverflow(callErr) {
			logger.Warn("structured summarize context overflow", "error", callErr)
			return backoff.Permanent(callErr)
		}
		return callErr
	}, retry.DefaultMaxRetries)
	if err != nil {
		return nil, fmt.Errorf("summarize failed after retries: %w", err)
	}
	return result, nil
}

func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "context") && !strings.Contains(msg, "token") {
		return false
	}
	patterns := []string{"context length", "context window", "maximum context", "too many tokens", "token limit", "prompt is too long", "context exceeded"}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

func (s *Summarizer) summarizeChunked(ctx context.Context, client llm.Client, preset config.Preset, transcript string, logger *slog.Logger) (string, string, string, error) {
	chunks := splitTranscript(transcript, s.chunkSizeTokens, s.chunkOverlapTokens)
	if len(chunks) == 0 {
		return guaranteedTitle(transcript), "", "{}", nil
	}

	chunkSummaries := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		messages := []llm.Message{
			{Role: "system", Content: "You are a strategic meeting analyst. Summarize this transcript chunk with key facts, decisions, and risks."},
			{Role: "user", Content: fmt.Sprintf("Chunk %d/%d\n\n%s", i+1, len(chunks), chunk)},
		}
		text, err := s.completeTextWithRetry(ctx, client, messages, logger)
		if err != nil {
			return "", "", "{}", fmt.Errorf("summarize chunk %d: %w", i+1, err)
		}
		chunkSummaries = append(chunkSummaries, strings.TrimSpace(text))
	}

	mergeTranscript := strings.Join(chunkSummaries, "\n\n")
	mergeMessages := s.structuredMessages(preset, mergeTranscript)
	mergeRaw, err := s.completeJSONWithRetry(ctx, client, mergeMessages, structuredSchema(), logger)
	if err != nil {
		return "", "", "{}", fmt.Errorf("merge chunk summaries: %w", err)
	}
	merged, parseErr := parseStructuredSummary(mergeRaw)
	if parseErr != nil {
		return guaranteedTitle(transcript), strings.TrimSpace(string(mergeRaw)), "{}", nil
	}
	if strings.TrimSpace(merged.Title) == "" {
		merged.Title = guaranteedTitle(transcript)
	}
	return strings.TrimSpace(merged.Title), strings.TrimSpace(merged.Summary), merged.speakerNamesJSON(), nil
}

func (s *Summarizer) completeTextWithRetry(ctx context.Context, client llm.Client, messages []llm.Message, logger *slog.Logger) (string, error) {
	var result string
	err := retry.Do(ctx, func() error {
		var callErr error
		result, callErr = client.Complete(ctx, messages)
		if callErr == nil {
			result = strings.TrimSpace(result)
		}
		return callErr
	}, retry.DefaultMaxRetries)
	if err != nil {
		return "", fmt.Errorf("chunk summarize failed after retries: %w", err)
	}
	return result, nil
}

func splitTranscript(transcript string, chunkSize, overlap int) []string {
	words := strings.Fields(transcript)
	if len(words) == 0 {
		return nil
	}
	if chunkSize <= 0 {
		chunkSize = 10_000
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= chunkSize {
		overlap = chunkSize / 2
	}

	chunks := make([]string, 0, (len(words)/chunkSize)+1)
	for start := 0; start < len(words); {
		end := start + chunkSize
		if end > len(words) {
			end = len(words)
		}
		chunks = append(chunks, strings.Join(words[start:end], " "))
		if end == len(words) {
			break
		}
		next := end - overlap
		if next <= start {
			next = start + 1
		}
		start = next
	}

	return chunks
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
