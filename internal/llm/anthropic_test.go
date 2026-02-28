package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicCompleteSeparatesSystemPrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")

		var req struct {
			Model     string `json:"model"`
			MaxTokens int64  `json:"max_tokens"`
			System    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"system"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if req.Model != "claude-3-5-sonnet-20240620" {
			t.Fatalf("unexpected model %q", req.Model)
		}
		if req.MaxTokens != 8192 {
			t.Fatalf("expected max_tokens 8192, got %d", req.MaxTokens)
		}
		if len(req.System) != 1 || req.System[0].Text != "be concise" {
			t.Fatalf("expected system prompt in top-level system field, got %#v", req.System)
		}
		if len(req.Messages) != 2 {
			t.Fatalf("expected 2 chat messages, got %d", len(req.Messages))
		}
		if req.Messages[0].Role != "user" || req.Messages[1].Role != "assistant" {
			t.Fatalf("unexpected chat roles: %#v", req.Messages)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_1",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-3-5-sonnet-20240620",
			"content": []map[string]any{
				{"type": "text", "text": " hello "},
				{"type": "text", "text": "world"},
			},
			"stop_reason":   "end_turn",
			"stop_sequence": "",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 2,
			},
		})
	}))
	defer server.Close()

	client, err := newAnthropicClient("test-key", "claude-3-5-sonnet-20240620", &clientOptions{baseURL: server.URL})
	if err != nil {
		t.Fatalf("newAnthropicClient failed: %v", err)
	}

	got, err := client.Complete(context.Background(), []Message{
		{Role: "system", Content: "be concise"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("expected combined trimmed text, got %q", got)
	}
}

func TestAnthropic_Complete_EmptyContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":            "msg_1",
			"type":          "message",
			"role":          "assistant",
			"model":         "claude-3-5-sonnet-20240620",
			"content":       []map[string]any{},
			"stop_reason":   "end_turn",
			"stop_sequence": "",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 0,
			},
		})
	}))
	defer server.Close()

	client, err := newAnthropicClient("test-key", "claude-3-5-sonnet-20240620", &clientOptions{baseURL: server.URL})
	if err != nil {
		t.Fatalf("newAnthropicClient failed: %v", err)
	}

	_, err = client.Complete(context.Background(), []Message{{Role: "user", Content: "hello"}})
	if err == nil {
		t.Fatal("expected error for empty content, got nil")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("expected 'empty response' in error, got %q", err.Error())
	}
}

func TestAnthropic_MaxTokens(t *testing.T) {
	var capturedMaxTokens int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			MaxTokens int64 `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		capturedMaxTokens = req.MaxTokens

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_1",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-3-5-sonnet-20240620",
			"content": []map[string]any{
				{"type": "text", "text": "ok"},
			},
			"stop_reason":   "end_turn",
			"stop_sequence": "",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 1,
			},
		})
	}))
	defer server.Close()

	client, err := newAnthropicClient("test-key", "claude-3-5-sonnet-20240620", &clientOptions{baseURL: server.URL})
	if err != nil {
		t.Fatalf("newAnthropicClient failed: %v", err)
	}

	_, err = client.Complete(context.Background(), []Message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if capturedMaxTokens != 8192 {
		t.Fatalf("expected max_tokens 8192, got %d", capturedMaxTokens)
	}
}
