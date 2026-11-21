package concierge

import (
	"math"
	"testing"

	"google.golang.org/genai"
)

// TestOpenAIGeneratorSendsTheTemperatureAndTheJSONShape fails if a request's
// temperature, 0 included, does not reach the chat completion as its
// temperature, or a JSON answer with a schema (ResponseMIMEType
// application/json and ResponseJsonSchema) does not reach it as a json_schema
// response_format, as ear-corpus's judge asks: the judge on an OpenAI API
// provider would be refused, or run at the API's default temperature, 1.
func TestOpenAIGeneratorSendsTheTemperatureAndTheJSONShape(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"verdict": map[string]any{"type": "string"}}}
	for _, c := range []struct {
		name        string
		temperature *float32
		want        float64
	}{
		{"temperature 0", genai.Ptr[float32](0), 0},
		{"temperature 0.2", genai.Ptr[float32](0.2), 0.2},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := newChatServer(t, []string{textEvent(`{"verdict":"yes"}`), finishEvent("stop")})
			generator := gatewayGenerator(t, srv.URL+"/v1")
			config := &genai.GenerateContentConfig{Temperature: c.temperature, ResponseMIMEType: "application/json", ResponseJsonSchema: schema}
			if _, err := streamed(generator, "claude-example", genai.Text("hi"), config); err != nil {
				t.Fatalf("a request with a temperature and a JSON schema: %v", err)
			}
			requests := srv.requests()
			if len(requests) != 1 {
				t.Fatalf("%d requests, want 1", len(requests))
			}
			body := requests[0]
			temperature, ok := body["temperature"].(float64)
			if !ok || math.Abs(temperature-c.want) > 1e-6 {
				t.Errorf("temperature %v (sent: %v), want %v", body["temperature"], ok, c.want)
			}
			format, _ := body["response_format"].(map[string]any)
			jsonSchema, _ := format["json_schema"].(map[string]any)
			sent, _ := jsonSchema["schema"].(map[string]any)
			if format["type"] != "json_schema" || sent["type"] != "object" || sent["properties"] == nil {
				t.Errorf("response_format %v, want json_schema with the request's schema", body["response_format"])
			}
		})
	}

	srv := newChatServer(t, []string{textEvent("ready"), finishEvent("stop")})
	generator := gatewayGenerator(t, srv.URL+"/v1")
	if _, err := streamed(generator, "claude-example", genai.Text("hi"), nil); err != nil {
		t.Fatal(err)
	}
	if body := srv.requests()[0]; body["temperature"] != nil || body["response_format"] != nil {
		t.Errorf("a request that sets neither sent temperature %v and response_format %v, want neither", body["temperature"], body["response_format"])
	}
}
