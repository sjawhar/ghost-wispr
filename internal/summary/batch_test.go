package summary

import (
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

func TestProcessBatchResultsMarksMissingModelVersion(t *testing.T) {
	s := New(config.Summarization{Model: "gemini/configured-summary-model"}, nil)
	job := &genai.BatchJob{Dest: &genai.BatchJobDestination{InlinedResponses: []*genai.InlinedResponse{{
		Metadata: map[string]string{"session_id": "session-1", "preset": "default"},
		Response: &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
			Content: &genai.Content{Parts: []*genai.Part{{Text: `{"title":"Title","summary":"Summary"}`}}},
		}}},
	}}}}

	results := s.ProcessBatchResults(job, []BatchSession{{ID: "session-1", Transcript: "meeting transcript"}})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if got := results[0].Model; got != llm.ModelNotReportedByProvider {
		t.Fatalf("model = %q, want %q", got, llm.ModelNotReportedByProvider)
	}
}

// A batch response's error keeps its code, by name, never its message:
// Gemini's message can quote the request, a session's transcript, and the
// error is logged and kept as the session's summary error.
func TestABatchResponseErrorKeepsOnlyItsCode(t *testing.T) {
	const planted = "quokkaplantedroomword"
	code := int32(3)
	s := New(config.Summarization{Model: "gemini/configured-summary-model"}, nil)
	job := &genai.BatchJob{Dest: &genai.BatchJobDestination{InlinedResponses: []*genai.InlinedResponse{{
		Metadata: map[string]string{"session_id": "session-1", "preset": "default"},
		Error:    &genai.JobError{Code: &code, Message: "Invalid content: " + planted, Details: []string{planted}},
	}}}}

	results := s.ProcessBatchResults(job, []BatchSession{{ID: "session-1", Transcript: "We agreed on " + planted + "."}})
	if len(results) != 1 || results[0].Error == nil {
		t.Fatalf("results = %+v, want one failed result", results)
	}
	if text := results[0].Error.Error(); strings.Contains(text, planted) || strings.Contains(text, "Invalid content") || !strings.Contains(text, "INVALID_ARGUMENT") {
		t.Fatalf("the error %q quotes Gemini's message or does not name code 3, INVALID_ARGUMENT", text)
	}
}
