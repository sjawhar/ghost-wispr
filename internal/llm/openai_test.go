package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

func TestOpenAIComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}

		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "gpt-4o-mini" {
			t.Fatalf("expected model gpt-4o-mini, got %q", req.Model)
		}
		if len(req.Messages) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(req.Messages))
		}
		if req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Fatalf("unexpected roles: %#v", req.Messages)
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
					"content": "  hello from openai  ",
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client, err := NewClient(keyProvider(t, config.APIOpenAI, server.URL+"/v1"), "gpt-4o-mini")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	got, err := client.Complete(context.Background(), []Message{{Role: "system", Content: "you are helpful"}, {Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got != "hello from openai" {
		t.Fatalf("expected trimmed response, got %q", got)
	}
}

func TestOpenAI_Complete_EmptyChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"created": 123,
			"model":   "gpt-4o-mini",
			"choices": []map[string]any{},
		})
	}))
	defer server.Close()

	client, err := NewClient(keyProvider(t, config.APIOpenAI, server.URL+"/v1"), "gpt-4o-mini")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	_, err = client.Complete(context.Background(), []Message{{Role: "user", Content: "hello"}})
	if err == nil {
		t.Fatal("expected error for empty choices, got nil")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("expected 'no choices' in error, got %q", err.Error())
	}
}
