package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// structuredFake is a gateway that lists sum-example and chat-example, and
// answers a chat completion as refuse decides from its response_format,
// keeping each completion's body.
type structuredFake struct {
	refuse func(format map[string]any) bool
	mu     sync.Mutex
	bodies map[string]map[string]any // model → its completion's body
}

func (f *structuredFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, `{"data":[{"id":"sum-example"},{"id":"chat-example"}]}`)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	model, _ := body["model"].(string)
	f.mu.Lock()
	f.bodies[model] = body
	f.mu.Unlock()
	format, _ := body["response_format"].(map[string]any)
	if format != nil && f.refuse(format) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"'additionalProperties' is required to be supplied and to be false for %s","type":"invalid_request_error","code":"400"}}`, model)
		return
	}
	_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`)
}

// structuredChecked checks a gateway at f whose summaries call sum-example
// and whose Concierge calls chat-example.
func structuredChecked(t *testing.T, f *structuredFake) (Status, string) {
	t.Helper()
	f.bodies = map[string]map[string]any{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p, _ := New(&config.Config{
		Providers:     map[string]config.Provider{"gateway": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}}},
		ProviderKeys:  map[string]string{"EXAMPLE_KEY": "sk-1"},
		Summarization: config.Summarization{Model: "gateway/sum-example"},
		Concierge:     config.Concierge{Model: "gateway/chat-example"},
	}, Options{}).Get("gateway")
	return p.Check(context.Background()), strings.TrimPrefix(srv.URL, "http://")
}

// strictMap says whether a response_format asks for strict output of a
// schema with a map property (an object whose additionalProperties is a
// schema), which OpenAI, and a gateway in front of it, refuse with 400.
func strictMap(format map[string]any) bool {
	schema, _ := format["json_schema"].(map[string]any)
	strict, _ := schema["strict"].(bool)
	inner, _ := schema["schema"].(map[string]any)
	properties, _ := inner["properties"].(map[string]any)
	for _, v := range properties {
		if property, _ := v.(map[string]any); property != nil {
			if _, isMap := property["additionalProperties"].(map[string]any); isMap && strict {
				return true
			}
		}
	}
	return false
}

// TestAGatewayCheckAsksForASummarysStructuredOutput fails if the check's
// call of a summarization model leaves out the structured output the
// summarizer asks for (a strict JSON schema of the summary's shape, whose
// speakers is a list of {id, name, confidence}), so a gateway that refuses
// every summary reads as working; or if the Concierge's call, whose client
// asks for none, carries one. internal/summary's
// TestTheProviderCheckAsksForTheSummarysSchema holds the copy to the
// summary's schema.
func TestAGatewayCheckAsksForASummarysStructuredOutput(t *testing.T) {
	t.Run("a gateway that takes it as the summarizer sends it", func(t *testing.T) {
		f := &structuredFake{refuse: strictMap}
		s, _ := structuredChecked(t, f)
		if !s.OK || !s.Asked {
			t.Fatalf("check %+v; want Works", s)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		format, _ := f.bodies["sum-example"]["response_format"].(map[string]any)
		schema, _ := format["json_schema"].(map[string]any)
		inner, _ := schema["schema"].(map[string]any)
		properties, _ := inner["properties"].(map[string]any)
		speakers, _ := properties["speakers"].(map[string]any)
		items, _ := speakers["items"].(map[string]any)
		if format["type"] != "json_schema" || schema["strict"] != true || speakers["type"] != "array" || fmt.Sprint(items["required"]) != "[confidence id name]" {
			t.Errorf("the summarization call's response_format = %v; want a strict json_schema whose speakers is a list of {id, name, confidence}", format)
		}
		if f.bodies["chat-example"]["response_format"] != nil {
			t.Errorf("the Concierge's call carries a response_format %v; its client sends none", f.bodies["chat-example"]["response_format"])
		}
	})
	t.Run("a gateway that refuses structured output", func(t *testing.T) {
		s, host := structuredChecked(t, &structuredFake{refuse: func(map[string]any) bool { return true }})
		if want := "Signed in, but " + host + " answered HTTP 400 (invalid_request_error) to a call of sum-example."; s.OK || !s.Asked || s.Message != want {
			t.Fatalf("check %+v; want Fails, saying %q", s, want)
		}
	})
}
