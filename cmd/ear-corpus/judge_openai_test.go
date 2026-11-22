package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestTheJudgeRunsOnAnOpenAIAPIProvider fails if -judge-only, with
// concierge.model on an OpenAI API provider (a model gateway), does not
// reach the provider's chat completions, as connectConcierge builds the
// client, with its JSON schema and no temperature, or does not read the
// verdict the model streams back. A temperature is refused there: the
// gateway's reasoning models (gpt-6.1-terra) answered every judge call at
// temperature 0 with HTTP 400, "Only temperature=1 is supported".
func TestTheJudgeRunsOnAnOpenAIAPIProvider(t *testing.T) {
	const keyEnv = "EXAMPLE_GATEWAY_KEY"
	t.Setenv(keyEnv, "sk-1")
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range []string{`{"content":"{\"conclusion_first\":\"yes\"}"}`, `{}`} {
			reason := ""
			if delta == `{}` {
				reason = `,"finish_reason":"stop"`
			}
			_, _ = fmt.Fprintf(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":%s%s}]}`+"\n\n", delta, reason)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	cfg := fmt.Sprintf("providers:\n  gateway: {api: openai, url: %s/v1, auth: {kind: api_key, env: %s}}\nconcierge:\n  model: gateway/claude-example\n", srv.URL, keyEnv)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"-config", path, "-judge-only", writeReplies(t, nil, []string{"It is merged."})}, &out, &errOut, connectConcierge)
	if code != exitFail {
		t.Fatalf("exit %d, want %d for a text the judge calls conclusion-first; stderr:\n%s", code, exitFail, errOut.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("%d chat completions, want 1; stderr:\n%s", len(bodies), errOut.String())
	}
	body := bodies[0]
	if temperature, sent := body["temperature"]; sent {
		t.Errorf("temperature %v sent, want none", temperature)
	}
	format, _ := body["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Errorf("response_format %v, want the judge's JSON schema", body["response_format"])
	}
	if body["model"] != "claude-example" {
		t.Errorf("model %v, want claude-example", body["model"])
	}
}
