package summary

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/retry"
)

// On the office Pi on 2026-11-16, after "Delete text", Gemini answered one
// summary request with {"title":"...","summary":"..."}. The unit stored that
// answer as the conversation's title and summary, which read "..." for about
// 15 minutes until a retry made a real one.
const answerWithNoText = `{"title":"...","summary":"..."}`

func summarizerFor(client *mockLLMClient) *Summarizer {
	cfg := config.Summarization{Model: "gemini/gemini-4-carbon", Presets: map[string]config.Preset{"default": {
		Description: "general", SystemPrompt: "system", UserTemplate: "{{transcript}}",
	}}}
	return New(cfg, func(_, _ string) (llm.Client, error) { return client, nil })
}

// TestSummarizeAsksAgainWhenTheAnswerHoldsNoSummaryText: an answer whose
// summary holds no word is not a summary; the summarizer asks again, as for a
// transient error, and stores the real answer.
func TestSummarizeAsksAgainWhenTheAnswerHoldsNoSummaryText(t *testing.T) {
	client := &mockLLMClient{jsonQueue: []mockJSONResult{
		{response: answerWithNoText},
		{response: `{"title":"Warehouse robot naming","summary":"The team named the warehouse robot project."}`},
	}}
	title, summaryText, _, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset: %v", err)
	}
	if title != "Warehouse robot naming" || summaryText != "The team named the warehouse robot project." {
		t.Errorf("title %q, summary %q; want the second answer", title, summaryText)
	}
	if client.calls != 2 {
		t.Errorf("the summarizer asked %d times, want 2", client.calls)
	}
}

// TestSummarizeFailsWhenEveryAnswerHoldsNoSummaryText: when every attempt
// answers with no summary text, the summary fails with that reason, which the
// session keeps and the UI shows beside a retry, instead of storing "...".
func TestSummarizeFailsWhenEveryAnswerHoldsNoSummaryText(t *testing.T) {
	client := &mockLLMClient{response: answerWithNoText}
	title, summaryText, _, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "default")
	if !errors.Is(err, errNoSummaryText) {
		t.Fatalf("SummarizeWithPreset = %q, %q, %v; want %v", title, summaryText, err, errNoSummaryText)
	}
	if client.calls != retry.DefaultMaxRetries {
		t.Errorf("the summarizer asked %d times, want %d", client.calls, retry.DefaultMaxRetries)
	}
}

// TestATitleWithNoTextIsMadeFromTheTranscript: a title with no word is
// treated as no title.
func TestATitleWithNoTextIsMadeFromTheTranscript(t *testing.T) {
	transcript := "We agreed to order the yard machine before the end of the month. " + buildTranscript(25)
	client := &mockLLMClient{response: `{"title":"…","summary":"The team agreed to order the yard machine."}`}
	title, summaryText, _, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset: %v", err)
	}
	if want := guaranteedTitle(transcript); title != want {
		t.Errorf("title %q, want %q", title, want)
	}
	if summaryText != "The team agreed to order the yard machine." {
		t.Errorf("summary %q", summaryText)
	}
}

// TestAChunkedSummaryAsksAgainWhenTheMergeHoldsNoSummaryText: a long
// transcript's merged summary is held to the same rule.
func TestAChunkedSummaryAsksAgainWhenTheMergeHoldsNoSummaryText(t *testing.T) {
	client := &mockLLMClient{
		textQueue: []mockTextResult{{response: "chunk one summary"}, {response: "chunk two summary"}},
		jsonQueue: []mockJSONResult{
			{response: answerWithNoText},
			{response: `{"title":"Merged Title","summary":"Merged summary"}`},
		},
	}
	s := summarizerFor(client)
	s.chunkTokenThreshold = 20
	s.chunkSizeTokens = 80
	s.chunkOverlapTokens = 20
	title, summaryText, _, err := s.SummarizeWithPreset(context.Background(), "session-1", buildTranscript(120), "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset: %v", err)
	}
	if title != "Merged Title" || summaryText != "Merged summary" {
		t.Errorf("title %q, summary %q; want the second merge answer", title, summaryText)
	}
}

// TestABatchAnswerWithNoSummaryTextFails: a batch summary answered with no
// summary text fails for its session, with that reason, rather than being
// stored.
func TestABatchAnswerWithNoSummaryTextFails(t *testing.T) {
	s := New(config.Summarization{Model: "gemini/gemini-4-carbon"}, nil)
	job := &genai.BatchJob{Dest: &genai.BatchJobDestination{InlinedResponses: []*genai.InlinedResponse{{
		Metadata: map[string]string{"session_id": "session-1", "preset": "default"},
		Response: &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
			Content: &genai.Content{Parts: []*genai.Part{{Text: answerWithNoText}}},
		}}},
	}}}}
	results := s.ProcessBatchResults(job, []BatchSession{{ID: "session-1", Transcript: buildTranscript(25)}})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if !errors.Is(results[0].Error, errNoSummaryText) || results[0].Summary != "" || results[0].Title != "" {
		t.Errorf("result = %+v; want the error %v and no title or summary", results[0], errNoSummaryText)
	}
}

// TestAChunkSummaryWithNoTextIsAskedAgain: a long transcript is summarized
// chunk by chunk, and a chunk answered with no text would drop that part of
// the conversation from the merged summary without an error. Such a chunk
// is asked again, as the merge is.
func TestAChunkSummaryWithNoTextIsAskedAgain(t *testing.T) {
	client := &mockLLMClient{
		textQueue: []mockTextResult{{response: "..."}, {response: "chunk one summary"}, {response: "chunk two summary"}},
		jsonQueue: []mockJSONResult{{response: `{"title":"Merged Title","summary":"Merged summary"}`}},
	}
	s := summarizerFor(client)
	s.chunkTokenThreshold = 20
	s.chunkSizeTokens = 80
	s.chunkOverlapTokens = 20
	if _, _, _, err := s.SummarizeWithPreset(context.Background(), "session-1", buildTranscript(120), "default"); err != nil {
		t.Fatalf("SummarizeWithPreset: %v", err)
	}
	if client.calls != 4 {
		t.Errorf("the summarizer asked %d times, want 4 (the empty chunk again, then the second chunk and the merge)", client.calls)
	}
	for _, message := range client.lastMessages {
		if strings.Contains(message.Content, "chunk one summary") && strings.Contains(message.Content, "\n...\n") {
			t.Errorf("the merge was given the chunk answered with no text: %q", message.Content)
		}
	}
}

// TestAChunkThatAlwaysAnswersWithNoTextFailsTheSummary: when every attempt
// at a chunk answers with no text, the summary fails with that reason, even
// though the merge would answer.
func TestAChunkThatAlwaysAnswersWithNoTextFailsTheSummary(t *testing.T) {
	client := &mockLLMClient{
		response:  "...",
		jsonQueue: []mockJSONResult{{response: `{"title":"Merged Title","summary":"Merged summary"}`}},
	}
	s := summarizerFor(client)
	s.chunkTokenThreshold = 20
	s.chunkSizeTokens = 80
	s.chunkOverlapTokens = 20
	if _, _, _, err := s.SummarizeWithPreset(context.Background(), "session-1", buildTranscript(120), "default"); !errors.Is(err, errNoSummaryText) {
		t.Fatalf("SummarizeWithPreset = %v, want %v", err, errNoSummaryText)
	}
}

// answersThatDoNotParse are structured answers whose title and summary hold
// no text, with a field that does not have the schema's type. The rule is
// for the title and the summary as the model wrote them, never for the raw
// answer, whose JSON keys hold letters.
var answersThatDoNotParse = []string{
	`{"title":"...","summary":"...","speakers":[]}`,
	`{"title":"...","summary":["..."]}`,
}

// TestAnAnswerWhoseFieldsHoldNoTextFailsEvenWhenItDoesNotParse: on the
// direct path.
func TestAnAnswerWhoseFieldsHoldNoTextFailsEvenWhenItDoesNotParse(t *testing.T) {
	for _, answer := range answersThatDoNotParse {
		client := &mockLLMClient{response: answer}
		title, summaryText, _, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "default")
		if !errors.Is(err, errNoSummaryText) {
			t.Errorf("answer %s: SummarizeWithPreset = %q, %q, %v; want %v", answer, title, summaryText, err, errNoSummaryText)
		}
	}
}

// TestABatchAnswerWhoseFieldsHoldNoTextFailsEvenWhenItDoesNotParse: on the
// batch path.
func TestABatchAnswerWhoseFieldsHoldNoTextFailsEvenWhenItDoesNotParse(t *testing.T) {
	s := New(config.Summarization{Model: "gemini/gemini-4-carbon"}, nil)
	for _, answer := range answersThatDoNotParse {
		job := &genai.BatchJob{Dest: &genai.BatchJobDestination{InlinedResponses: []*genai.InlinedResponse{{
			Metadata: map[string]string{"session_id": "session-1", "preset": "default"},
			Response: &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
				Content: &genai.Content{Parts: []*genai.Part{{Text: answer}}},
			}}},
		}}}}
		results := s.ProcessBatchResults(job, []BatchSession{{ID: "session-1", Transcript: buildTranscript(25)}})
		if len(results) != 1 || !errors.Is(results[0].Error, errNoSummaryText) || results[0].Summary != "" {
			t.Errorf("answer %s: results = %+v; want one, failed with %v and no summary", answer, results, errNoSummaryText)
		}
	}
}

// TestAnAnswerWithAMalformedSpeakersFieldKeepsItsTitleAndSummary: a speakers
// field of the wrong type does not throw away a real title and summary.
func TestAnAnswerWithAMalformedSpeakersFieldKeepsItsTitleAndSummary(t *testing.T) {
	client := &mockLLMClient{response: `{"title":"Warehouse robot naming","summary":"The team named the project.","speakers":[]}`}
	title, summaryText, speakerNames, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "default")
	if err != nil {
		t.Fatalf("SummarizeWithPreset: %v", err)
	}
	if title != "Warehouse robot naming" || summaryText != "The team named the project." || speakerNames != "{}" {
		t.Errorf("title %q, summary %q, speakers %s; want the answer's title and summary and no speakers", title, summaryText, speakerNames)
	}
}

// TestAPlainTextAnswerIsTheSummary: an answer that is not a JSON object is
// the summary as written.
func TestAPlainTextAnswerIsTheSummary(t *testing.T) {
	client := &mockLLMClient{response: "The team named the warehouse robot project."}
	_, summaryText, _, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "default")
	if err != nil || summaryText != "The team named the warehouse robot project." {
		t.Errorf("SummarizeWithPreset = %q, %v; want the answer as the summary", summaryText, err)
	}
}
