package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertGeminiMessages(t *testing.T) {
	systemInstruction, contents := convertGeminiMessages([]Message{
		{Role: "system", Content: "follow policy"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "user", Content: "how are you"},
	})

	if systemInstruction == nil {
		t.Fatalf("expected system instruction, got nil")
	}
	if len(systemInstruction.Parts) != 1 || systemInstruction.Parts[0].Text != "follow policy" {
		t.Fatalf("unexpected system instruction: %#v", systemInstruction)
	}

	if len(contents) != 3 {
		t.Fatalf("expected 3 conversation messages, got %d", len(contents))
	}
	if contents[0].Role != "user" || contents[0].Parts[0].Text != "hello" {
		t.Fatalf("unexpected first message: %#v", contents[0])
	}
	if contents[1].Role != "model" || contents[1].Parts[0].Text != "hi there" {
		t.Fatalf("unexpected second message: %#v", contents[1])
	}
	if contents[2].Role != "user" || contents[2].Parts[0].Text != "how are you" {
		t.Fatalf("unexpected third message: %#v", contents[2])
	}
}

func TestGemini_Complete_EmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{
					"content": map[string]any{
						"parts": []map[string]any{
							{"text": ""},
						},
						"role": "model",
					},
					"finishReason": "STOP",
				},
			},
		})
	}))
	defer server.Close()

	client, err := newGeminiClient("test-key", "gemini-test", &clientOptions{baseURL: server.URL})
	if err != nil {
		t.Fatalf("newGeminiClient failed: %v", err)
	}

	_, err = client.Complete(context.Background(), []Message{
		{Role: "user", Content: "hello"},
	})
	if err == nil {
		t.Fatal("expected error for empty result, got nil")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("expected 'empty response' in error, got %q", err.Error())
	}
}
