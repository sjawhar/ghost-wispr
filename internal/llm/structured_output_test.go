package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// mapShaped is a schema whose speakers property is a map keyed by speaker
// id, which OpenAI's strict structured output cannot express.
var mapShaped = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"title": map[string]any{"type": "string"},
		"speakers": map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
		},
	},
	"required":             []string{"title"},
	"additionalProperties": false,
}

// strictShaped is a schema strict structured output takes.
var strictShaped = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"title": map[string]any{"type": "string"}},
	"required":             []string{"title"},
	"additionalProperties": false,
}

// TestAnOpenAIClientAsksForStrictOutputOnlyWithASchemaStrictModeTakes fails
// if an OpenAI API client sends strict: true with a schema strict mode
// refuses (one with a map), so that every such request on an OpenAI API
// provider, a model gateway's or api.openai.com's, is refused with 400; or
// if it gives up strict output for a schema strict mode takes, as the
// summary's is (internal/summary's TestTheSummarySchemaIsStrict).
func TestAnOpenAIClientAsksForStrictOutputOnlyWithASchemaStrictModeTakes(t *testing.T) {
	var mu sync.Mutex
	var sent []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ResponseFormat struct {
				JSONSchema struct {
					Schema map[string]any `json:"schema"`
					Strict bool           `json:"strict"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		sent = append(sent, request.ResponseFormat.JSONSchema.Strict)
		mu.Unlock()
		properties, _ := request.ResponseFormat.JSONSchema.Schema["properties"].(map[string]any)
		if request.ResponseFormat.JSONSchema.Strict && properties["speakers"] != nil {
			// What OpenAI and a gateway in front of it answer.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"'additionalProperties' is required to be supplied and to be false","type":"invalid_request_error","code":"400"}}`)
			return
		}
		chatCompletion(w, `{"title":"Deploy moved to Friday"}`)
	}))
	defer srv.Close()
	client, err := NewClient(keyProvider(t, config.APIOpenAI, srv.URL), "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	messages := []Message{{Role: "user", Content: "summarize"}}
	if raw, err := client.CompleteJSON(context.Background(), messages, mapShaped); err != nil || string(raw) != `{"title":"Deploy moved to Friday"}` {
		t.Fatalf("CompleteJSON with a map schema = %s, %v; want the answer", raw, err)
	}
	if _, err := client.CompleteJSON(context.Background(), messages, strictShaped); err != nil {
		t.Fatalf("CompleteJSON with a strict schema: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0] || !sent[1] {
		t.Fatalf("strict sent %v; want false for a map schema and true for a strict schema", sent)
	}
}
