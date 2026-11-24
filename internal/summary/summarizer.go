package summary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cenkalti/backoff/v5"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/retry"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type ClientFactory func(provider, model string) (llm.Client, error)

type Summarizer struct {
	cfg                 config.Summarization
	factory             ClientFactory
	OnResult            func(err error)
	router              *Router
	sleep               func(time.Duration)
	now                 func() time.Time
	chunkTokenThreshold int
	chunkSizeTokens     int
	chunkOverlapTokens  int
	callTimeout         time.Duration // bounds each model call (callTimeout)
}

const (
	defaultMinSummaryWords = 20
	tokenPerCharEstimate   = 4
)

// callTimeout bounds each model call a summary makes (the preset's choice,
// a chunk's summary, the summary); the caller's context still ends a call
// sooner. The OpenAI and Anthropic clients cap an answer at 8192 tokens,
// which a model writing 46 tokens a second ends in three minutes, and the
// summaries the unit makes take seconds; a call still out after three
// minutes has stopped answering or is running away, as claude-haiku-4-5
// through the model gateway did for 6 min 42 s on 2026-11-20 while the
// backlog behind it waited. The attempt fails, and is asked again as any
// transient failure is.
const callTimeout = 3 * time.Minute

// errNoSummaryText is a model's answer whose summary holds no letter or
// digit: an empty summary, or "..." as Gemini once answered on the office Pi.
// It is not a summary, so the summarizer asks again, and fails with it when
// every attempt answers so.
var errNoSummaryText = errors.New("the model answered with no summary text")

// errBrokenJSONAnswer is a model's answer that begins as a JSON object but
// does not parse: one cut off at its length limit, as the runaway answer of
// 2026-11-20 was, or broken. It is no summary (the raw JSON would be stored
// as one), so the summarizer asks again, and fails with it when every
// attempt answers so. Its text names no limit or size: the retry and the
// overflow check read an error's words.
var errBrokenJSONAnswer = errors.New("the model's answer begins as a JSON object but does not parse")

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
		callTimeout:         callTimeout,
	}
}

// SummaryResult holds a completed summary and the model reported by the
// provider that generated it.
type SummaryResult struct {
	Title        string
	Summary      string
	Preset       string
	SpeakerNames string
	Model        string
}

func (s *Summarizer) Summarize(ctx context.Context, sessionID, transcript string) (title, summaryText, presetName, speakerNames string, err error) {
	result, err := s.SummarizeResult(ctx, sessionID, transcript)
	return result.Title, result.Summary, result.Preset, result.SpeakerNames, err
}

// SummarizeReported returns the provider model beside the summary, so callers
// can persist the exact result without a shared mutable result cache.
func (s *Summarizer) SummarizeReported(ctx context.Context, sessionID, transcript string) (title, summaryText, presetName, speakerNames, model string, err error) {
	result, err := s.SummarizeResult(ctx, sessionID, transcript)
	return result.Title, result.Summary, result.Preset, result.SpeakerNames, result.Model, err
}

// SummarizeResult summarizes transcript with the preset the router chooses. A
// transcript too short to summarize gets its title alone, with the preset the
// router falls back to: no model is asked which preset fits it.
func (s *Summarizer) SummarizeResult(ctx context.Context, sessionID, transcript string) (result SummaryResult, err error) {
	defer func() {
		if s.OnResult != nil {
			s.OnResult(err)
		}
	}()

	presetName := s.fallbackPreset()
	if !s.tooShort(transcript) {
		if presetName, err = s.selectPreset(ctx, transcript); err != nil {
			return SummaryResult{}, fmt.Errorf("select preset: %w", err)
		}
	}
	result, err = s.SummarizeWithPresetResult(ctx, sessionID, transcript, presetName)
	result.Preset = presetName
	return result, err
}

func (s *Summarizer) SummarizeWithPreset(ctx context.Context, sessionID, transcript, presetName string) (title, summaryText, speakerNames string, err error) {
	result, err := s.SummarizeWithPresetResult(ctx, sessionID, transcript, presetName)
	return result.Title, result.Summary, result.SpeakerNames, err
}

// SummarizeWithPresetReported returns the provider model with a preset
// summary. It is the direct counterpart to SummarizeWithPreset.
func (s *Summarizer) SummarizeWithPresetReported(ctx context.Context, sessionID, transcript, presetName string) (title, summaryText, speakerNames, model string, err error) {
	result, err := s.SummarizeWithPresetResult(ctx, sessionID, transcript, presetName)
	return result.Title, result.Summary, result.SpeakerNames, result.Model, err
}

func (s *Summarizer) SummarizeWithPresetResult(ctx context.Context, _ string, transcript, presetName string) (result SummaryResult, err error) {
	defer func() {
		if s.OnResult != nil {
			s.OnResult(err)
		}
	}()

	if s.tooShort(transcript) {
		return SummaryResult{Title: guaranteedTitle(transcript), SpeakerNames: "{}"}, nil
	}

	preset, ok := s.cfg.Presets[presetName]
	if !ok {
		return SummaryResult{}, fmt.Errorf("unknown preset %q", presetName)
	}

	modelStr := preset.Model
	if modelStr == "" {
		modelStr = s.cfg.Model
	}

	provider, modelName, err := config.ParseModelRef(modelStr)
	if err != nil {
		return SummaryResult{}, err
	}

	client, err := s.factory(provider, modelName)
	if err != nil {
		return SummaryResult{}, fmt.Errorf("create llm client: %w", err)
	}

	logger := logging.WithModule(logging.FromContext(ctx, nil), "summary")
	if s.shouldChunk(transcript) {
		title, summaryText, speakerNames, err := s.summarizeChunked(ctx, client, preset, transcript, logger)
		if err != nil {
			return SummaryResult{}, err
		}
		return SummaryResult{Title: title, Summary: summaryText, Preset: presetName, SpeakerNames: speakerNames, Model: reportedModel(client)}, nil
	}

	messages := s.BuildPromptMessages(preset, transcript)
	parsed, err := s.completeSummaryWithRetry(ctx, client, messages, transcript, logger)
	if err != nil {
		if isContextOverflow(err) {
			title, summaryText, speakerNames, chunkErr := s.summarizeChunked(ctx, client, preset, transcript, logger)
			if chunkErr == nil {
				return SummaryResult{Title: title, Summary: summaryText, Preset: presetName, SpeakerNames: speakerNames, Model: reportedModel(client)}, nil
			}
			return SummaryResult{}, fmt.Errorf("summarize chunked after overflow: %w", chunkErr)
		}
		return SummaryResult{}, err
	}
	return SummaryResult{
		Title:        parsed.Title,
		Summary:      parsed.Summary,
		Preset:       presetName,
		SpeakerNames: parsed.speakerNamesJSON(),
		Model:        reportedModel(client),
	}, nil
}

func reportedModel(client llm.Client) string {
	reporter, ok := client.(interface{ ReportedModel() string })
	if !ok {
		return ""
	}
	return strings.TrimSpace(reporter.ReportedModel())
}

type parsedSummary struct {
	Title    string                         `json:"title"`
	Summary  string                         `json:"summary"`
	Speakers map[string]speakerNameMetadata `json:"speakers,omitempty"`
}

// speakerNameMetadata is a speaker's name, as the session stores it under
// the speaker's id.
type speakerNameMetadata struct {
	Name       string `json:"name"`
	Confidence string `json:"confidence"`
}

// speakerEntry is one speaker of the schema's speakers list.
type speakerEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Confidence string `json:"confidence"`
}

func parseStructuredSummary(raw json.RawMessage) (parsedSummary, error) {
	var envelope struct {
		Title    string          `json:"title"`
		Summary  string          `json:"summary"`
		Speakers json.RawMessage `json:"speakers,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return parsedSummary{}, err
	}

	parsed := parsedSummary{
		Title:   envelope.Title,
		Summary: envelope.Summary,
	}

	speakers, err := parseSpeakersField(envelope.Speakers)
	if err != nil {
		return parsedSummary{}, err
	}
	parsed.Speakers = speakers

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

// summaryAnswer reads a model's structured answer (raw) as a title, a summary
// and speaker names. An answer that is not JSON at all is the summary as
// written; one that begins as a JSON object but does not parse, such as an
// answer cut off at its length limit, is errBrokenJSONAnswer. In a JSON
// object, the title and the summary are its "title" and "summary" strings,
// also when another field (such as "speakers") does not have the schema's
// type; a field that is not a string reads as empty, so an answer such as
// {"title":"...","summary":["..."]} has no summary text, and the raw JSON,
// whose keys hold letters, is never taken for one. A title that holds no
// word (meaningfulText), such as "...", is made from the transcript instead
// (guaranteedTitle).
func summaryAnswer(raw json.RawMessage, transcript string) (parsedSummary, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		text := strings.TrimSpace(string(raw))
		if strings.HasPrefix(text, "{") {
			return parsedSummary{}, errBrokenJSONAnswer
		}
		return parsedSummary{Title: guaranteedTitle(transcript), Summary: text}, nil
	}
	parsed, err := parseStructuredSummary(raw)
	if err != nil {
		parsed = parsedSummary{Title: jsonString(fields["title"]), Summary: jsonString(fields["summary"])}
	}
	parsed.Title = strings.TrimSpace(parsed.Title)
	parsed.Summary = strings.TrimSpace(parsed.Summary)
	if !meaningfulText(parsed.Title) {
		parsed.Title = guaranteedTitle(transcript)
	}
	return parsed, nil
}

// jsonString returns the JSON string raw holds, or "" when it holds another
// value or nothing.
func jsonString(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// meaningfulText reports whether text holds a letter or a digit.
func meaningfulText(text string) bool {
	return strings.IndexFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// parseSpeakersField reads an answer's speakers, in any shape a model gave
// them: the schema's list of {id, name, confidence} (decodeSpeakers), the map
// keyed by speaker id the schema asked for before (which a batch job asked
// before the change still answers in), or either as a JSON string.
func parseSpeakersField(raw json.RawMessage) (map[string]speakerNameMetadata, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if speakers, err := decodeSpeakers(raw); err == nil {
		return speakers, nil
	}

	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, err
	}
	encoded = strings.TrimSpace(encoded)
	if encoded == "" || encoded == "null" {
		return nil, nil
	}
	return decodeSpeakers([]byte(encoded))
}

// decodeSpeakers reads speakers as the schema's list or as the map keyed by
// speaker id, into that map, which the session stores and the API and the
// page read. A list entry with no id or no name names no one.
func decodeSpeakers(data []byte) (map[string]speakerNameMetadata, error) {
	var list []speakerEntry
	if err := json.Unmarshal(data, &list); err == nil {
		speakers := make(map[string]speakerNameMetadata, len(list))
		for _, entry := range list {
			id := strings.TrimSpace(entry.ID)
			if id == "" || strings.TrimSpace(entry.Name) == "" {
				continue
			}
			speakers[id] = speakerNameMetadata{Name: entry.Name, Confidence: entry.Confidence}
		}
		return speakers, nil
	}
	var speakers map[string]speakerNameMetadata
	if err := json.Unmarshal(data, &speakers); err != nil {
		return nil, err
	}
	return speakers, nil
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

func (s *Summarizer) BuildPromptMessages(preset config.Preset, transcript string) []llm.Message {
	rendered := strings.ReplaceAll(preset.UserTemplate, "{{transcript}}", transcript)
	rendered = strings.ReplaceAll(rendered, "{{date}}", s.now().UTC().Format("2006-01-02"))

	system := strings.TrimSpace(strings.Join([]string{
		"You are the memory of a business. You process transcripts from an always-on",
		"recorder that captures everything \u2014 meetings, quick conversations, instructions,",
		"solo thinking, phone calls, and ambient moments.",
		"",
		"Your job: make every moment findable and legible later. Someone will search for",
		"this in a week, a month, a year. The title is how they find it. The summary is",
		"how they remember what happened without re-reading the transcript.",
		"",
		"Adapt your depth to the content:",
		"- A brief instruction or aside \u2192 clear title + one-line summary",
		"- A casual conversation \u2192 title + brief description of what was discussed",
		"- A working session \u2192 title + key context, decisions, and outcomes",
		"- A formal meeting \u2192 title + structured summary with sections",
		"",
		"The title describes WHAT happened, not how the transcript starts. Never quote",
		"the transcript verbatim as the title.",
		"",
		"Return only valid JSON matching the provided schema.",
		strings.TrimSpace(preset.SystemPrompt),
	}, "\n"))

	user := strings.TrimSpace(strings.Join([]string{
		"Summarize this transcript. Match your depth to the content — a brief exchange",
		"needs only a sentence, a long working session deserves structured sections.",
		"",
		"Output requirements:",
		`- title: 4-10 words describing the topic, specific and non-empty`,
		`- summary: markdown — for short content just a sentence or two; for substantial content start with a one-line bottom line, then use sections (## Decisions, ## Key Outcomes, ## Risks/Notes)`,
		`- speakers: a list with one entry for each speaker whose name you know, each {"id":"<the number of the speaker's label in the transcript: 0 for Speaker 0>","name":"<name>","confidence":"mentioned|inferred"}; [] when you know no name. Use 'mentioned' if the name was said, 'inferred' if guessed from context.`,
		"Respond as JSON only.",
		"",
		"Examples:",
		"",
		`Brief exchange:`,
		`{"title":"Deploy moved to Friday","summary":"Quick decision to push the deploy from Thursday to Friday to give QA more time.","speakers":[]}`,
		"",
		`Working session:`,
		`{"title":"Q2 roadmap alignment","summary":"Team aligned on Q2 roadmap and sequencing.\n\n## Decisions\n- Ship feature flags before rollout.\n\n## Key Outcomes\n- Roadmap finalized and communicated.\n\n## Risks/Notes\n- API dependency may delay launch by one sprint.","speakers":[{"id":"0","name":"Ben","confidence":"mentioned"}]}`,

		"",
		"Transcript:",
		rendered,
	}, "\n"))

	return []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
}

// StructuredSchema is the schema a summary is asked in. Strict structured
// output takes it (jsonschema.Strict): every object lists each of its
// properties as required and allows no other. So speakers is a list of
// {id, name, confidence}, not a map keyed by speaker id, which strict mode
// refuses and which Anthropic models through a model gateway answered as {}
// every time; summaryAnswer reads the list into that map, which the session
// stores. internal/providers' check asks a summarization model in a copy of
// it (summaryShapedSchema).
func StructuredSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "Concise descriptive title in 4-10 words. Describes the topic, never quotes the transcript.",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "Markdown summary. Brief for short content. For substantial content: one-line bottom line first, then ## Decisions, ## Key Outcomes, ## Risks/Notes sections.",
			},
			"speakers": map[string]any{
				"type":        "array",
				"description": "One entry for each speaker whose name is known; empty when none is.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":   map[string]any{"type": "string", "description": `The speaker's ID in the transcript, such as "0".`},
						"name": map[string]any{"type": "string"},
						"confidence": map[string]any{
							"type": "string",
							"enum": []string{"mentioned", "inferred"},
						},
					},
					"required":             []string{"confidence", "id", "name"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"speakers", "summary", "title"},
		"additionalProperties": false,
	}
}

// withCallTimeout makes one model call (do) with a deadline of its own,
// timeout, inside ctx, and says so when that deadline cut it off.
func withCallTimeout(ctx context.Context, timeout time.Duration, do func(context.Context) error) error {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := do(callCtx)
	if err != nil && ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("the model did not answer within %s: %w", timeout, err)
	}
	return err
}

// completeSummaryWithRetry asks client for the structured summary messages
// ask for, each call within the call timeout, retrying a transient failure,
// and reads the answer (summaryAnswer; transcript gives the title an answer
// lacks). An answer whose summary holds no word (errNoSummaryText), or that
// begins as a JSON object but does not parse (errBrokenJSONAnswer), is asked
// again in the same way, and when every attempt answers so the summary fails
// with that reason rather than storing it.
func (s *Summarizer) completeSummaryWithRetry(ctx context.Context, client llm.Client, messages []llm.Message, transcript string, logger *slog.Logger) (parsedSummary, error) {
	var result parsedSummary
	err := retry.Do(ctx, func() error {
		var raw json.RawMessage
		callErr := withCallTimeout(ctx, s.callTimeout, func(ctx context.Context) (err error) {
			raw, err = client.CompleteJSON(ctx, messages, StructuredSchema())
			return err
		})
		if callErr != nil {
			if isContextOverflow(callErr) {
				logger.Warn("structured summarize context overflow", "error", callErr)
				return backoff.Permanent(callErr)
			}
			return callErr
		}
		parsed, err := summaryAnswer(raw, transcript)
		if err != nil {
			// The answer can quote the conversation: the log keeps its
			// length, never its text.
			logger.Warn("the model's answer begins as a JSON object but does not parse; asking again", "answer_bytes", len(raw))
			return err
		}
		result = parsed
		if !meaningfulText(result.Summary) {
			// Safe to log and keep in the error: a summary that holds no
			// letter or digit (meaningfulText), such as "...", holds none
			// of the conversation's words.
			logger.Warn("the model answered with no summary text; asking again", "summary", result.Summary)
			return fmt.Errorf("%w: %q", errNoSummaryText, result.Summary)
		}
		return nil
	}, retry.DefaultMaxRetries)
	if err != nil {
		return parsedSummary{}, fmt.Errorf("summarize failed after retries: %w", err)
	}
	return result, nil
}

// isContextOverflow reports whether err says the transcript is longer than
// the model takes: a provider's error says so in TooLong (its message is not
// kept), any other error in its text.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	var answered *providererr.Error
	if errors.As(err, &answered) {
		return answered.TooLong
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
	mergeMessages := s.BuildPromptMessages(preset, mergeTranscript)
	merged, err := s.completeSummaryWithRetry(ctx, client, mergeMessages, transcript, logger)
	if err != nil {
		return "", "", "{}", fmt.Errorf("merge chunk summaries: %w", err)
	}
	return merged.Title, merged.Summary, merged.speakerNamesJSON(), nil
}

// completeTextWithRetry asks client for the summary of one chunk of a long
// transcript, each call within the call timeout, retrying a transient
// failure. An answer that holds no word (errNoSummaryText) would drop that
// part of the conversation from the merged summary without an error, so it
// is asked again in the same way, and when every attempt answers so the
// summary fails with that reason.
func (s *Summarizer) completeTextWithRetry(ctx context.Context, client llm.Client, messages []llm.Message, logger *slog.Logger) (string, error) {
	var result string
	err := retry.Do(ctx, func() error {
		callErr := withCallTimeout(ctx, s.callTimeout, func(ctx context.Context) (err error) {
			result, err = client.Complete(ctx, messages)
			return err
		})
		if callErr != nil {
			return callErr
		}
		result = strings.TrimSpace(result)
		if !meaningfulText(result) {
			// Safe to log: it holds no letter or digit, so no words.
			logger.Warn("the model answered a chunk with no summary text; asking again", "summary", result)
			return fmt.Errorf("%w: %q", errNoSummaryText, result)
		}
		return nil
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
		return s.fallbackPreset(), nil
	}
	return s.router.SelectPreset(ctx, transcript)
}

// fallbackPreset is the preset a summary takes when no model chooses one: the
// router's fallback, else the one preset there is, else "default".
func (s *Summarizer) fallbackPreset() string {
	if s.router != nil {
		return s.router.fallbackPreset()
	}
	for name := range s.cfg.Presets {
		return name
	}
	return "default"
}

// tooShort says whether transcript has fewer words of conversation than a
// summary needs (MinSummaryWords); its speaker labels are not words of it.
func (s *Summarizer) tooShort(transcript string) bool {
	minWords := s.cfg.MinSummaryWords
	if minWords <= 0 {
		minWords = defaultMinSummaryWords
	}
	return len(strings.Fields(transcribe.WithoutSpeakerLabels(transcript))) < minWords
}

func (s *Summarizer) Presets() map[string]config.Preset {
	return s.cfg.Presets
}
