package summary

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

type mockStore struct {
	claimFn func(sessionID, promptHash string) (bool, error)
}

func (m mockStore) ClaimSummaryRequest(sessionID, promptHash string) (bool, error) {
	return m.claimFn(sessionID, promptHash)
}

func TestSummarizeReturnsMarkdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"created": 123,
			"model":   "gpt-4o-mini",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "## Summary\n- Key decision made",
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"

	summarizer := NewOpenAIWithConfig(config, "gpt-4o-mini", mockStore{claimFn: func(_, _ string) (bool, error) {
		return true, nil
	}})
	summarizer.sleep = func(_ time.Duration) {}

	text := strings.Repeat("hello ", 25)
	got, err := summarizer.Summarize(context.Background(), "s1", text)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}
	if !strings.Contains(got, "## Summary") {
		t.Fatalf("unexpected summary: %q", got)
	}
}

func TestSummarizeSkipsShortTranscript(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"

	summarizer := NewOpenAIWithConfig(config, "gpt-4o-mini", nil)
	got, err := summarizer.Summarize(context.Background(), "s2", "too short")
	if err != nil {
		t.Fatalf("Summarize returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty summary, got %q", got)
	}
	if calls.Load() != 0 {
		t.Fatalf("expected zero OpenAI calls, got %d", calls.Load())
	}
}

func TestSummarizeRetriesOnFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limit"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-2",
			"object":  "chat.completion",
			"created": 123,
			"model":   "gpt-4o-mini",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "retry success",
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"

	summarizer := NewOpenAIWithConfig(config, "gpt-4o-mini", mockStore{claimFn: func(_, _ string) (bool, error) {
		return true, nil
	}})
	summarizer.sleep = func(_ time.Duration) {}

	text := strings.Repeat("token ", 30)
	got, err := summarizer.Summarize(context.Background(), "s3", text)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}
	if got != "retry success" {
		t.Fatalf("expected retry success summary, got %q", got)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestSummarizeIdempotencySkipsDuplicate(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}}})
	}))
	defer server.Close()

	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"

	store := mockStore{claimFn: func(_, _ string) (bool, error) {
		return false, nil
	}}
	summarizer := NewOpenAIWithConfig(config, "gpt-4o-mini", store)

	text := strings.Repeat("token ", 30)
	got, err := summarizer.Summarize(context.Background(), "s4", text)
	if err != nil {
		t.Fatalf("Summarize returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty summary for duplicate request, got %q", got)
	}
	if calls.Load() != 0 {
		t.Fatalf("expected zero API calls for duplicate, got %d", calls.Load())
	}
}
