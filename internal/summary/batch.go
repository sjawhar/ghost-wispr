package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// BatchSession holds the data needed to build a batch request for one session.
type BatchSession struct {
	ID         string
	Transcript string
}

// BatchResult holds the parsed result for a single session from a batch job.
type BatchResult struct {
	SessionID    string
	Title        string
	Summary      string
	SpeakerNames string
	PresetName   string
	Error        error
}

// BuildBatchRequests builds InlinedRequests for a set of sessions.
// Each request includes the session_id and preset name in metadata for
// correlation when processing results.
func (s *Summarizer) BuildBatchRequests(ctx context.Context, sessions []BatchSession) ([]*genai.InlinedRequest, error) {
	requests := make([]*genai.InlinedRequest, 0, len(sessions))
	schema := StructuredSchema()

	for _, sess := range sessions {
		presetName, err := s.selectPreset(ctx, sess.Transcript)
		if err != nil {
			return nil, fmt.Errorf("select preset for session %s: %w", sess.ID, err)
		}

		preset, ok := s.cfg.Presets[presetName]
		if !ok {
			return nil, fmt.Errorf("unknown preset %q for session %s", presetName, sess.ID)
		}

		messages := s.BuildPromptMessages(preset, sess.Transcript)

		// Convert llm.Message → genai types (same logic as internal/llm/gemini.go)
		var systemInstruction *genai.Content
		var contents []*genai.Content
		for _, m := range messages {
			switch m.Role {
			case "system":
				systemInstruction = &genai.Content{Parts: []*genai.Part{{Text: m.Content}}}
			case "user":
				contents = append(contents, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: m.Content}}})
			case "assistant":
				contents = append(contents, &genai.Content{Role: "model", Parts: []*genai.Part{{Text: m.Content}}})
			}
		}

		requests = append(requests, &genai.InlinedRequest{
			Contents: contents,
			Config: &genai.GenerateContentConfig{
				SystemInstruction:  systemInstruction,
				ResponseMIMEType:   "application/json",
				ResponseJsonSchema: schema,
			},
			Metadata: map[string]string{
				"session_id": sess.ID,
				"preset":     presetName,
			},
		})
	}

	return requests, nil
}

// ProcessBatchResults extracts and parses results from a completed batch job.
// Responses are matched to sessions via metadata (with index fallback).
func (s *Summarizer) ProcessBatchResults(job *genai.BatchJob, sessions []BatchSession) []BatchResult {
	if job.Dest == nil {
		return nil
	}

	responses := job.Dest.InlinedResponses
	results := make([]BatchResult, len(responses))

	// Build session lookup by ID for metadata-based matching.
	sessionByID := make(map[string]BatchSession, len(sessions))
	for _, sess := range sessions {
		sessionByID[sess.ID] = sess
	}

	for i, resp := range responses {
		var result BatchResult

		// Get session info from metadata (primary) or index (fallback).
		if resp.Metadata != nil {
			result.SessionID = resp.Metadata["session_id"]
			result.PresetName = resp.Metadata["preset"]
		}
		if result.SessionID == "" && i < len(sessions) {
			result.SessionID = sessions[i].ID
		}

		if resp.Error != nil {
			result.Error = fmt.Errorf("batch response error: %s", resp.Error.Message)
			results[i] = result
			continue
		}

		if resp.Response == nil {
			result.Error = fmt.Errorf("nil response for session %s", result.SessionID)
			results[i] = result
			continue
		}

		text := strings.TrimSpace(resp.Response.Text())
		if text == "" {
			result.Error = fmt.Errorf("empty response for session %s", result.SessionID)
			results[i] = result
			continue
		}

		parsed, err := parseStructuredSummary(json.RawMessage(text))
		if err != nil {
			// Fall back to raw text as summary.
			sess := sessionByID[result.SessionID]
			result.Title = guaranteedTitle(sess.Transcript)
			result.Summary = text
			result.SpeakerNames = "{}"
			results[i] = result
			continue
		}

		if strings.TrimSpace(parsed.Title) == "" {
			sess := sessionByID[result.SessionID]
			parsed.Title = guaranteedTitle(sess.Transcript)
		}

		result.Title = strings.TrimSpace(parsed.Title)
		result.Summary = strings.TrimSpace(parsed.Summary)
		result.SpeakerNames = parsed.speakerNamesJSON()
		results[i] = result
	}

	return results
}
