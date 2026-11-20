package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestAnOpenAIClientCapsTheAnswer fails if an OpenAI API client's Complete
// or CompleteJSON asks with no limit on the answer's tokens, or with any but
// max_completion_tokens 8192, the limit the Anthropic client sends: with
// none, claude-haiku-4-5 through the model gateway wrote a summary of
// 64,529 characters for 6 min 42 s. max_tokens, which OpenAI's reasoning
// models refuse, is never sent.
func TestAnOpenAIClientCapsTheAnswer(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		chatCompletion(w, `{"answer":"ok"}`)
	}))
	defer srv.Close()
	client, err := NewClient(gatewayProvider(t, config.APIOpenAI, srv.URL+"/v1", &idTokens{}), "anthropic/claude-test")
	if err != nil {
		t.Fatal(err)
	}
	messages := []Message{{Role: "user", Content: "summarize"}}
	if _, err := client.Complete(context.Background(), messages); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := client.CompleteJSON(context.Background(), messages, JSONSchema(map[string]string{"answer": "string"})); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(bodies))
	}
	for i, call := range []string{"Complete", "CompleteJSON"} {
		if bodies[i]["max_completion_tokens"] != float64(8192) || bodies[i]["max_tokens"] != nil {
			t.Errorf("%s asked with max_completion_tokens %v and max_tokens %v; want 8192 and none", call, bodies[i]["max_completion_tokens"], bodies[i]["max_tokens"])
		}
	}
}

// TestTheOpenAIAndAnthropicClientsReportTheAnswersModel fails if an OpenAI
// API or Anthropic API client does not report the model its last answer
// named, as neither did, so a summary through a model gateway stored
// summary_model "": each reports it as the Gemini client reports its
// modelVersion, ModelNotReportedByProvider for an answer that names none,
// and "" before any answer.
func TestTheOpenAIAndAnthropicClientsReportTheAnswersModel(t *testing.T) {
	for _, tc := range []struct {
		name, api, answered, want string
	}{
		{"openai, named", config.APIOpenAI, "gpt-test-2026", "gpt-test-2026"},
		{"openai, not named", config.APIOpenAI, "", ModelNotReportedByProvider},
		{"anthropic, named", config.APIAnthropic, "claude-test-2026", "claude-test-2026"},
		{"anthropic, not named", config.APIAnthropic, "", ModelNotReportedByProvider},
	} {
		for _, call := range []string{"Complete", "CompleteJSON"} {
			t.Run(tc.name+", "+call, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if tc.api == config.APIAnthropic {
						_ = json.NewEncoder(w).Encode(map[string]any{
							"id": "msg_1", "type": "message", "role": "assistant", "model": tc.answered,
							"content":     []map[string]any{{"type": "text", "text": `{"answer":"ok"}`}},
							"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
						})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "chatcmpl-1", "object": "chat.completion", "created": 123, "model": tc.answered,
						"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": `{"answer":"ok"}`}, "finish_reason": "stop"}},
					})
				}))
				defer srv.Close()
				url := srv.URL
				if tc.api == config.APIOpenAI {
					url += "/v1"
				}
				client, err := NewClient(keyProvider(t, tc.api, url), "configured-model")
				if err != nil {
					t.Fatal(err)
				}
				reporter, ok := client.(interface{ ReportedModel() string })
				if !ok {
					t.Fatalf("the %s client reports no model", tc.api)
				}
				if got := reporter.ReportedModel(); got != "" {
					t.Fatalf("ReportedModel() = %q before any answer, want \"\"", got)
				}
				messages := []Message{{Role: "user", Content: "summarize"}}
				if call == "Complete" {
					_, err = client.Complete(context.Background(), messages)
				} else {
					_, err = client.CompleteJSON(context.Background(), messages, JSONSchema(map[string]string{"answer": "string"}))
				}
				if err != nil {
					t.Fatalf("%s: %v", call, err)
				}
				if got := reporter.ReportedModel(); got != tc.want {
					t.Fatalf("ReportedModel() = %q after an answer naming %q, want %q (not the configured model)", got, tc.answered, tc.want)
				}
			})
		}
	}
}
