package summary

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/jsonschema"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// TestTheSummarySchemaIsStrict fails if the schema a summary is asked in is
// one OpenAI's strict structured output refuses (jsonschema.Strict), as a
// speakers map keyed by speaker id was: an OpenAI API provider then got
// strict: false, and claude-haiku-4-5 through the model gateway answered
// speakers {} every time (INFRA-4853, round 4). Its speakers must be a list
// of {id, name, confidence}, and the prompt's examples must answer in it.
func TestTheSummarySchemaIsStrict(t *testing.T) {
	schema := StructuredSchema()
	if !jsonschema.Strict(schema) {
		t.Fatalf("jsonschema.Strict(StructuredSchema()) = false; want a schema strict structured output takes: %v", schema)
	}
	properties, _ := schema["properties"].(map[string]any)
	speakers, _ := properties["speakers"].(map[string]any)
	items, _ := speakers["items"].(map[string]any)
	fields, _ := items["properties"].(map[string]any)
	if speakers["type"] != "array" || items["type"] != "object" || len(fields) != 3 || fields["id"] == nil || fields["name"] == nil || fields["confidence"] == nil {
		t.Fatalf("speakers = %v; want a list of objects {id, name, confidence}", speakers)
	}
	s := New(config.Summarization{}, nil)
	user := s.BuildPromptMessages(config.Preset{UserTemplate: "{{transcript}}"}, "Speaker 0: hi")[1].Content
	if strings.Contains(user, `"speakers":{`) || !strings.Contains(user, `"speakers":[{"id":"0","name":"Ben","confidence":"mentioned"}]`) {
		t.Fatalf("the prompt's examples do not answer speakers as the schema's list:\n%s", user)
	}
}

// TestASpeakersListIsStoredAsTheSpeakerMap fails if an answer whose
// speakers is the schema's list of {id, name, confidence} is not stored as
// the map keyed by speaker id that the session keeps and the API and the
// page read, on the direct path or a batch's; or if an answer in the map
// shape the schema asked for before (a cached or batch answer) is no longer
// read. A list entry with no id or no name names no one.
func TestASpeakersListIsStoredAsTheSpeakerMap(t *testing.T) {
	const want = `{"0":{"name":"Alex","confidence":"mentioned"},"1":{"name":"Sam","confidence":"inferred"}}`
	for name, speakers := range map[string]string{
		"the schema's list":         `[{"id":"0","name":"Alex","confidence":"mentioned"},{"id":"1","name":"Sam","confidence":"inferred"},{"id":"2","name":" ","confidence":"inferred"},{"id":"","name":"Kim","confidence":"mentioned"}]`,
		"the list as a JSON string": `"[{\"id\":\"0\",\"name\":\"Alex\",\"confidence\":\"mentioned\"},{\"id\":\"1\",\"name\":\"Sam\",\"confidence\":\"inferred\"}]"`,
		"the old map":               `{"0":{"name":"Alex","confidence":"mentioned"},"1":{"name":"Sam","confidence":"inferred"}}`,
	} {
		answer := `{"title":"Launch moved to Friday","summary":"The launch moves to Friday.","speakers":` + speakers + `}`
		t.Run(name, func(t *testing.T) {
			client := &mockLLMClient{response: answer}
			_, _, speakerNames, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", buildTranscript(25), "default")
			if err != nil || speakerNames != want {
				t.Errorf("speaker names %s, %v; want %s", speakerNames, err, want)
			}
			job := &genai.BatchJob{Dest: &genai.BatchJobDestination{InlinedResponses: []*genai.InlinedResponse{{
				Metadata: map[string]string{"session_id": "session-1", "preset": "default"},
				Response: &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{{Text: answer}}}}}},
			}}}}
			results := New(config.Summarization{Model: "gemini/gemini-4-carbon"}, nil).ProcessBatchResults(job, []BatchSession{{ID: "session-1", Transcript: buildTranscript(25)}})
			if len(results) != 1 || results[0].Error != nil || results[0].SpeakerNames != want {
				t.Errorf("batch results %+v; want speaker names %s", results, want)
			}
		})
	}
}

// TestTheProviderCheckAsksForTheSummarysSchema fails if the settings page's
// check of a summarization model on an OpenAI API provider asks for
// structured output in any schema but the summary's (descriptions aside), or
// with another strict: the check copies the schema (providers cannot import
// this package), and a provider that refuses the summary's schema refuses
// every summary, so a copy that drifts proves nothing.
func TestTheProviderCheckAsksForTheSummarysSchema(t *testing.T) {
	var (
		mu     sync.Mutex
		format map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"sum-example"}]}`))
			return
		}
		var body struct {
			ResponseFormat struct {
				JSONSchema map[string]any `json:"json_schema"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		format = body.ResponseFormat.JSONSchema
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p, err := providers.New(&config.Config{
		Providers:     map[string]config.Provider{"gateway": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_KEY"}}},
		ProviderKeys:  map[string]string{"EXAMPLE_KEY": "sk-1"},
		Summarization: config.Summarization{Model: "gateway/sum-example"},
	}, providers.Options{}).Get("gateway")
	if err != nil {
		t.Fatal(err)
	}
	if s := p.Check(context.Background()); !s.OK || !s.Asked {
		t.Fatalf("check %+v; want Works", s)
	}
	mu.Lock()
	defer mu.Unlock()
	want := withoutDescriptions(roundTrip(t, StructuredSchema()))
	if got := roundTrip(t, format["schema"]); !reflect.DeepEqual(got, want) {
		t.Errorf("the check asks in the schema\n%v\nwant the summary's, descriptions aside:\n%v", got, want)
	}
	if format["strict"] != jsonschema.Strict(StructuredSchema()) {
		t.Errorf("the check asks with strict %v; the summarizer's client sends %v", format["strict"], jsonschema.Strict(StructuredSchema()))
	}
}

// roundTrip is v as JSON decodes it.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// withoutDescriptions is a decoded schema with every "description" removed.
func withoutDescriptions(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, sub := range v {
			if k != "description" {
				out[k] = withoutDescriptions(sub)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, sub := range v {
			out[i] = withoutDescriptions(sub)
		}
		return out
	default:
		return v
	}
}
