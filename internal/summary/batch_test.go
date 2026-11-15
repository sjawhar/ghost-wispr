package summary

import (
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
