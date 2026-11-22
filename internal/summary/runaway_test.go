package summary

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

// runawayAnswer has the shape of the answer claude-haiku-4-5 gave a summary
// through the model gateway on 2026-11-20, after 6 min 42 s: a JSON object
// cut off inside its summary, which ".jl" repeated filled to the end. The
// summarizer stored all 64,529 characters of it as the summary.
var runawayAnswer = `{"title":"Embeddings move to the gateway","summary":"Team decided` + strings.Repeat(".jl", 2000)

// TestAnAnswerCutOffInsideItsJSONIsNotStored fails if an answer that begins
// as a JSON object but does not parse is stored whole as the summary, as the
// runaway answer was: that attempt fails and the summarizer asks again, and
// when every attempt answers so the summary fails, with an error that quotes
// none of the answer. A batch's answer is held to the same rule. An answer
// that is not JSON at all is still the summary (TestAPlainTextAnswerIsTheSummary).
func TestAnAnswerCutOffInsideItsJSONIsNotStored(t *testing.T) {
	t.Run("asked again", func(t *testing.T) {
		client := &mockLLMClient{jsonQueue: []mockJSONResult{
			{response: runawayAnswer},
			{response: `{"title":"Embeddings move to the gateway","summary":"The team moves the embeddings to the gateway."}`},
		}}
		_, summaryText, _, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "default")
		if err != nil || summaryText != "The team moves the embeddings to the gateway." || client.calls != 2 {
			t.Fatalf("SummarizeWithPreset = %d-byte summary %.60q, %v after %d calls; want the second answer's summary after 2", len(summaryText), summaryText, err, client.calls)
		}
	})
	t.Run("every attempt cut off", func(t *testing.T) {
		client := &mockLLMClient{response: runawayAnswer}
		_, summaryText, _, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "default")
		if !errors.Is(err, errBrokenJSONAnswer) || summaryText != "" {
			t.Fatalf("SummarizeWithPreset = %d-byte summary, %v; want no summary and %v", len(summaryText), err, errBrokenJSONAnswer)
		}
		if strings.Contains(err.Error(), "Team decided") || strings.Contains(err.Error(), ".jl") {
			t.Fatalf("the error quotes the answer: %.200q", err.Error())
		}
	})
	t.Run("in a batch", func(t *testing.T) {
		job := &genai.BatchJob{Dest: &genai.BatchJobDestination{InlinedResponses: []*genai.InlinedResponse{{
			Metadata: map[string]string{"session_id": "session-1", "preset": "default"},
			Response: &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{{Text: runawayAnswer}}}}}},
		}}}}
		results := New(config.Summarization{Model: "gemini/gemini-4-carbon"}, nil).ProcessBatchResults(job, []BatchSession{{ID: "session-1", Transcript: buildTranscript(25)}})
		if len(results) != 1 {
			t.Fatalf("%d batch results, want 1", len(results))
		}
		if !errors.Is(results[0].Error, errBrokenJSONAnswer) || results[0].Summary != "" {
			t.Fatalf("batch result: error %v, %d-byte summary; want %v and no summary", results[0].Error, len(results[0].Summary), errBrokenJSONAnswer)
		}
	})
}

// stallingClient answers each of its first stall calls only when the call's
// context ends, as a provider that stops answering does, and every later
// call with answer. It keeps the time each call had left when it started:
// -1 for a call with no deadline.
type stallingClient struct {
	mu     sync.Mutex
	stall  int
	answer string
	left   []time.Duration
}

func (c *stallingClient) call(ctx context.Context) (string, error) {
	c.mu.Lock()
	left := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline)
	}
	c.left = append(c.left, left)
	stall := len(c.left) <= c.stall
	c.mu.Unlock()
	if stall {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return c.answer, nil
}

func (c *stallingClient) Complete(ctx context.Context, _ []llm.Message) (string, error) {
	return c.call(ctx)
}

func (c *stallingClient) CompleteJSON(ctx context.Context, _ []llm.Message, _ map[string]any) (json.RawMessage, error) {
	answer, err := c.call(ctx)
	return json.RawMessage(answer), err
}

func (c *stallingClient) deadlines() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.left...)
}

// TestEachSummaryCallHasItsOwnDeadline fails if a summary's model call can
// run for as long as the provider keeps it open, as the runaway answer ran
// for 6 min 42 s while the backlog behind it waited: each call (the preset's
// choice, a chunk's summary, the summary) ends at the summarizer's call
// timeout, and the attempt fails and is asked again. It also fails if the
// caller's context no longer ends a call sooner.
func TestEachSummaryCallHasItsOwnDeadline(t *testing.T) {
	const timeout = 50 * time.Millisecond
	const answer = `{"title":"Embeddings move to the gateway","summary":"The team moves the embeddings to the gateway."}`
	presets := map[string]config.Preset{"default": {UserTemplate: "{{transcript}}"}}
	for _, tc := range []struct {
		name    string
		presets map[string]config.Preset
		words   int
		chunked bool
		calls   int // the calls made: the stalled one, and each after it
	}{
		{name: "the summary", presets: presets, words: 25, calls: 2},
		{name: "a chunk's summary", presets: presets, words: 60, chunked: true, calls: 4}, // chunk 1 stalled and asked again, chunk 2, the merge
		{name: "the preset's choice", presets: map[string]config.Preset{"default": {UserTemplate: "{{transcript}}"}, "other": {UserTemplate: "{{transcript}}"}}, words: 25, calls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &stallingClient{stall: 1, answer: answer}
			s := New(config.Summarization{Model: "gateway/openai/gpt-test", Presets: tc.presets}, func(_, _ string) (llm.Client, error) { return client, nil })
			s.callTimeout = timeout
			if s.router != nil {
				s.router.callTimeout = timeout
			}
			if tc.chunked {
				s.chunkTokenThreshold, s.chunkSizeTokens, s.chunkOverlapTokens = 10, 40, 20
			}
			// The test's own bound: without a deadline of its own, the
			// stalled call would end only here.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := s.SummarizeResult(ctx, "session-1", buildTranscript(tc.words))
			left := client.deadlines()
			if err != nil || result.Summary != "The team moves the embeddings to the gateway." || len(left) != tc.calls {
				t.Fatalf("SummarizeResult = %q, %v after calls with %v left; want the answer after %d calls", result.Summary, err, left, tc.calls)
			}
			for i, l := range left {
				if l <= 0 || l > timeout {
					t.Errorf("call %d started with %v left; want a deadline of its own, at most %v", i+1, l, timeout)
				}
			}
		})
	}
	t.Run("the caller's context", func(t *testing.T) {
		client := &stallingClient{stall: 1 << 20, answer: answer}
		s := summarizerForClient(client)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		started := time.Now()
		_, _, _, err := s.SummarizeWithPreset(ctx, "session-1", buildTranscript(25), "default")
		if !errors.Is(err, context.Canceled) || time.Since(started) > 2*time.Second {
			t.Fatalf("SummarizeWithPreset = %v after %v; want context.Canceled at once", err, time.Since(started))
		}
	})
}

// summarizerForClient is summarizerFor with any client.
func summarizerForClient(client llm.Client) *Summarizer {
	return New(config.Summarization{Model: "gemini/gemini-4-carbon", Presets: map[string]config.Preset{"default": {UserTemplate: "{{transcript}}"}}},
		func(_, _ string) (llm.Client, error) { return client, nil })
}
