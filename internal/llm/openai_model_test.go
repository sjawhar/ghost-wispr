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

// TestAnOpenAIClientSendsTheModelAsGiven fails if an OpenAI API client's
// Complete or CompleteJSON sends any model but the one it was given: a model
// gateway such as LiteLLM names a model with a slash ("openai/gpt-test"), so a
// client that strips what comes before the slash, as the provider/model of a
// role is split, asks the gateway for a model it does not serve.
func TestAnOpenAIClientSendsTheModelAsGiven(t *testing.T) {
	var (
		mu     sync.Mutex
		models []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		models = append(models, body.Model)
		mu.Unlock()
		chatCompletion(w, `{"answer":"ok"}`)
	}))
	defer srv.Close()
	client, err := NewClient(gatewayProvider(t, config.APIOpenAI, srv.URL+"/v1", &idTokens{}), "openai/gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	messages := []Message{{Role: "user", Content: "ping"}}

	if _, err := client.Complete(context.Background(), messages); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := client.CompleteJSON(context.Background(), messages, JSONSchema(map[string]string{"answer": "string"})); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(models) != 2 || models[0] != "openai/gpt-test" || models[1] != "openai/gpt-test" {
		t.Fatalf("Complete and CompleteJSON sent the models %q; want openai/gpt-test from each", models)
	}
}
