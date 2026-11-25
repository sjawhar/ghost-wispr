package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// gatewayProvider is a model gateway at url that speaks the OpenAI API, signed
// in by the Google ID token "id-1" for its audience.
func gatewayProvider(t *testing.T, url string) *providers.Provider {
	t.Helper()
	mint := func(context.Context, string, string) (string, error) { return "id-1", nil }
	cfg := &config.Config{Providers: map[string]config.Provider{
		"gateway": {API: config.APIOpenAI, URL: url, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
	}}
	p, err := providers.New(cfg, providers.Options{IDToken: mint}).Get("gateway")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// gatewayGenerator is the Concierge's model client on gatewayProvider.
func gatewayGenerator(t *testing.T, url string) Generator {
	t.Helper()
	generator, err := NewGenerator(context.Background(), gatewayProvider(t, url))
	if err != nil {
		t.Fatal(err)
	}
	return generator
}

// chatServer is a fake OpenAI API whose /v1/chat/completions streams each
// call's scripted events as server-sent events, and keeps each call's body
// and Authorization header.
type chatServer struct {
	*httptest.Server
	mu     sync.Mutex
	turns  [][]string
	bodies []map[string]any
	auths  []string
}

func newChatServer(t *testing.T, turns ...[]string) *chatServer {
	t.Helper()
	s := &chatServer{turns: turns}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *chatServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	call := len(s.bodies)
	s.bodies = append(s.bodies, body)
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	var events []string
	if call < len(s.turns) {
		events = s.turns[call]
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		w.(http.Flusher).Flush()
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// requests are the bodies of the calls made so far.
func (s *chatServer) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

// chunkEvent is a chat completion chunk whose one choice carries delta, and
// finishes for reason when it is set.
func chunkEvent(delta map[string]any, reason string) string {
	choice := map[string]any{"index": 0, "delta": delta}
	if reason != "" {
		choice["finish_reason"] = reason
	}
	encoded, err := json.Marshal(map[string]any{"id": "chunk-1", "object": "chat.completion.chunk", "choices": []any{choice}})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func textEvent(s string) string { return chunkEvent(map[string]any{"content": s}, "") }

// toolEvent is a piece of the tool call at index: its id and name, when set,
// and a piece of its arguments.
func toolEvent(index int, id, name, args string) string {
	function := map[string]any{"arguments": args}
	if name != "" {
		function["name"] = name
	}
	toolCall := map[string]any{"index": index, "function": function}
	if id != "" {
		toolCall["id"], toolCall["type"] = id, "function"
	}
	return chunkEvent(map[string]any{"tool_calls": []any{toolCall}}, "")
}

func finishEvent(reason string) string { return chunkEvent(map[string]any{}, reason) }

// streamed runs one call and returns the parts it streamed, in order, and the
// error that ended it.
func streamed(generator Generator, model string, contents []*genai.Content, config *genai.GenerateContentConfig) ([]*genai.Part, error) {
	var parts []*genai.Part
	for response, err := range generator.GenerateContentStream(context.Background(), model, contents, config) {
		if err != nil {
			return parts, err
		}
		parts = append(parts, responseParts(response)...)
	}
	return parts, nil
}

// decoded is JSON text as encoding/json reads it into an any.
func decoded(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestOpenAIGeneratorTranslatesATurnBothWays fails if a turn does not reach an
// OpenAI API provider as chat completions (the model, a stream, the system
// message, the tools, the model's own tool call and the tool message that
// answers it under the same id, and the provider's credential), or the
// streamed answer does not come back as genai parts: the text as it arrives,
// in order, then one function call with its whole arguments, though they came
// split across chunks.
func TestOpenAIGeneratorTranslatesATurnBothWays(t *testing.T) {
	srv := newChatServer(t, []string{
		chunkEvent(map[string]any{"role": "assistant"}, ""),
		textEvent("ABC-12 is "),
		textEvent("done."),
		toolEvent(0, "call-b", "issue_holder", `{"ke`),
		toolEvent(0, "", "", `y":"ABC-12"}`),
		finishEvent("tool_calls"),
	})
	schema := map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}, "required": []any{"key"}}
	contents := []*genai.Content{
		genai.NewContentFromText("what about ABC-12", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{thought("Read it first."), text("Reading."), call("call-a", "read_issue", map[string]any{"key": "ABC-12"}, "sig-1")}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "call-a", Name: "read_issue", Response: map[string]any{"status": "done"}}}}},
	}
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("the rules", genai.RoleUser),
		ThinkingConfig:    &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelLow},
		Tools:             []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "read_issue", Description: "Read one issue.", ParametersJsonSchema: schema}}}},
	}
	parts, err := streamed(gatewayGenerator(t, srv.URL+"/v1"), "openai/gpt-example", contents, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var texts []string
	var calls []*genai.FunctionCall
	for _, part := range parts {
		switch {
		case part.FunctionCall != nil:
			calls = append(calls, part.FunctionCall)
		case len(calls) > 0:
			t.Errorf("the part %+v came after the function call", part)
		default:
			texts = append(texts, part.Text)
		}
	}
	if !reflect.DeepEqual(texts, []string{"ABC-12 is ", "done."}) {
		t.Errorf("texts %q, want each delta's text in order", texts)
	}
	want := &genai.FunctionCall{ID: "call-b", Name: "issue_holder", Args: map[string]any{"key": "ABC-12"}}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Errorf("function calls %+v, want one, %+v", calls, want)
	}

	requests := srv.requests()
	if len(requests) != 1 {
		t.Fatalf("%d requests, want 1", len(requests))
	}
	body := requests[0]
	if body["model"] != "openai/gpt-example" || body["stream"] != true {
		t.Errorf("model %v, stream %v; want openai/gpt-example, streamed", body["model"], body["stream"])
	}
	wantMessages := decoded(t, `[
		{"role": "system", "content": "the rules"},
		{"role": "user", "content": "what about ABC-12"},
		{"role": "assistant", "content": "Reading.", "tool_calls": [{"id": "call-a", "type": "function", "function": {"name": "read_issue", "arguments": "{\"key\":\"ABC-12\"}"}}]},
		{"role": "tool", "tool_call_id": "call-a", "content": "{\"status\":\"done\"}"}
	]`)
	if !reflect.DeepEqual(body["messages"], wantMessages) {
		t.Errorf("messages %v,\nwant %v", body["messages"], wantMessages)
	}
	wantTools := decoded(t, `[{"type": "function", "function": {"name": "read_issue", "description": "Read one issue.",
		"parameters": {"type": "object", "properties": {"key": {"type": "string"}}, "required": ["key"]}}}]`)
	if !reflect.DeepEqual(body["tools"], wantTools) {
		t.Errorf("tools %v,\nwant %v", body["tools"], wantTools)
	}
	// reasoning_effort is refused by the OpenAI API's models that do not
	// reason, so the thinking level is not sent; tool_choice is auto.
	for _, absent := range []string{"tool_choice", "reasoning_effort"} {
		if v, ok := body[absent]; ok {
			t.Errorf("%s = %v, want it not sent", absent, v)
		}
	}
	if srv.auths[0] != "Bearer id-1" {
		t.Errorf("Authorization = %q, want the provider's Google ID token", srv.auths[0])
	}
}

// TestOpenAIGeneratorTellsTheModelToCallNoTool fails if genai's function
// calling mode NONE, the Concierge's last round, does not reach the OpenAI
// API as tool_choice none, or reaches it with no tools, which the OpenAI API
// refuses.
func TestOpenAIGeneratorTellsTheModelToCallNoTool(t *testing.T) {
	srv := newChatServer(t, []string{textEvent("ok"), finishEvent("stop")}, []string{textEvent("ok"), finishEvent("stop")})
	generator := gatewayGenerator(t, srv.URL+"/v1")
	none := &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}}
	tools := []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "list_joined_agents", ParametersJsonSchema: map[string]any{"type": "object"}}}}}
	for _, cfg := range []*genai.GenerateContentConfig{{Tools: tools, ToolConfig: none}, {ToolConfig: none}} {
		if _, err := streamed(generator, "claude-example", genai.Text("hi"), cfg); err != nil {
			t.Fatal(err)
		}
	}
	requests := srv.requests()
	if len(requests) != 2 {
		t.Fatalf("%d requests, want 2", len(requests))
	}
	if requests[0]["tool_choice"] != "none" {
		t.Errorf("with tools, tool_choice = %v, want none", requests[0]["tool_choice"])
	}
	if v, ok := requests[1]["tool_choice"]; ok {
		t.Errorf("with no tools, tool_choice = %v, want it not sent", v)
	}
}

// TestOpenAIGeneratorFailsAStreamCutShort fails if a stream that ends before
// the model finished, with no finish_reason, reads as a whole answer: its
// last sentence would be spoken cut, and a tool call's arguments may not be
// whole. No function call of it comes back.
func TestOpenAIGeneratorFailsAStreamCutShort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+textEvent("ABC-12 is")+"\n\ndata: "+toolEvent(0, "call-a", "read_issue", `{"key":"ABC-12"}`)+"\n\n")
	}))
	defer srv.Close()
	parts, err := streamed(gatewayGenerator(t, srv.URL+"/v1"), "claude-example", genai.Text("hi"), nil)
	if err == nil || !strings.Contains(err.Error(), "before the model finished") {
		t.Errorf("err = %v, want a stream cut short", err)
	}
	for _, part := range parts {
		if part.FunctionCall != nil {
			t.Errorf("a cut stream's tool call came back: %+v", part.FunctionCall)
		}
	}
}

// TestOpenAIGeneratorRefusesWhatItCannotTranslate fails if a setting or a
// part the OpenAI API has no place for is dropped without a word: the model
// would run with settings other than the caller's. Each refusal names what it
// cannot translate, and no request is sent.
func TestOpenAIGeneratorRefusesWhatItCannotTranslate(t *testing.T) {
	srv := newChatServer(t)
	generator := gatewayGenerator(t, srv.URL+"/v1")
	mime := "text/csv"
	hi := genai.Text("hi")
	for name, c := range map[string]struct {
		contents []*genai.Content
		config   *genai.GenerateContentConfig
		want     string
	}{
		"an answer of another type": {hi, &genai.GenerateContentConfig{ResponseMIMEType: mime}, "ResponseMIMEType"},
		"Google Search":             {hi, &genai.GenerateContentConfig{Tools: []*genai.Tool{{GoogleSearch: &genai.GoogleSearch{}}}}, "GoogleSearch"},
		"a genai.Schema":            {hi, &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "f", Parameters: &genai.Schema{Type: genai.TypeObject}}}}}}, "Parameters"},
		"a mode of ANY":             {hi, &genai.GenerateContentConfig{ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}}}, "ANY"},
		"an image":                  {[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1}}}}}}, nil, "InlineData"},
		"a call with no id":         {[]*genai.Content{{Role: genai.RoleModel, Parts: []*genai.Part{call("", "read_issue", nil, "")}}}, nil, "id"},
		"an answer with no id":      {[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "read_issue"}}}}}, nil, "id"},
		"a call from the user":      {[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{call("call-a", "read_issue", nil, "")}}}, nil, "user"},
		"a role of its own":         {[]*genai.Content{{Role: "narrator", Parts: []*genai.Part{text("hi")}}}, nil, "narrator"},
		"an answer of the model":    {[]*genai.Content{{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "call-a", Name: "read_issue"}}}}}, nil, "model"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := streamed(generator, "claude-example", c.contents, c.config)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want a refusal naming %s", err, c.want)
			}
		})
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("%d requests sent, want none", n)
	}
}

// TestOpenAIGeneratorWithholdsTheProvidersWords fails if a refusal, as the
// answer to the request or as an error event in its stream, comes back with
// the provider's message, which can quote the room's words, or without the
// HTTP status and the provider's error code.
func TestOpenAIGeneratorWithholdsTheProvidersWords(t *testing.T) {
	const roomWord = "pineapple"
	refusal := `{"error": {"message": "the request said ` + roomWord + `", "type": "invalid_request_error", "code": "example_refusal"}}`
	for name, c := range map[string]struct {
		serve http.HandlerFunc
		want  []string
	}{
		"an answer of 400": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, refusal)
		}, []string{"HTTP 400", "example_refusal"}},
		"an error event": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+textEvent("Partly")+"\n\ndata: "+refusal+"\n\n")
		}, []string{"example_refusal"}},
		"a tool call whose name is words": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+toolEvent(0, "", "tell them "+roomWord, `{"key":`)+"\n\ndata: "+finishEvent("tool_calls")+"\n\n")
		}, []string{"no JSON object"}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(c.serve)
			defer srv.Close()
			_, err := streamed(gatewayGenerator(t, srv.URL+"/v1"), "claude-example", genai.Text("what did they say about "+roomWord), nil)
			if err == nil {
				t.Fatal("no error, want the refusal")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
			if strings.Contains(err.Error(), roomWord) {
				t.Errorf("error %q holds the provider's message", err)
			}
			var reduced *providererr.Error
			if !errors.As(err, &reduced) || reduced.Provider != "gateway" {
				t.Errorf("error %q is no providererr.Error of the provider gateway, which the Concierge's log keeps as it is", err)
			}
		})
	}
}

// TestTheConciergeAnswersOnAnOpenAIAPIProvider fails if the Concierge cannot
// run a turn on an OpenAI API provider, such as a model gateway, end to end:
// the model calls a tool, with no id, as some providers send it; the tool's
// answer goes back as a tool message under the id the generator gave the
// call; and the model's answer is spoken sentence by sentence.
func TestTheConciergeAnswersOnAnOpenAIAPIProvider(t *testing.T) {
	srv := newChatServer(t,
		[]string{toolEvent(0, "", "search_issues", `{"query":`), toolEvent(0, "", "", `"voice work"}`), finishEvent("tool_calls")},
		[]string{textEvent("ABC-12 is in progress. It was updat"), textEvent("ed this morning."), finishEvent("stop")},
	)
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession("meeting-1", meetingStart); err != nil {
		t.Fatal(err)
	}
	records := &fakeRecords{
		hits:     []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Status: "in_progress", Score: 0.9}},
		issues:   map[string]*dispatch.Issue{},
		events:   map[string][]dispatch.Event{},
		docs:     map[string]string{},
		projects: []string{"ABC"},
	}
	c, err := New(&Config{
		Model:     "claude-example",
		Generator: gatewayGenerator(t, srv.URL+"/v1"),
		Records:   records,
		Store:     store,
		PublicURL: "https://unit.example",
		Now:       func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	c.SetAgents(&fakeAgents{id: "unit-session", states: map[string]envoy.SessionInfo{}})
	eventually(t, "the project list was read", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.projects != nil
	})

	exchange := voice.Exchange{SessionID: "meeting-1", WakeAt: testNow.Add(-8 * time.Second), AskerTurn: "what is happening with the voice work"}
	var spoken []said
	if err := c.Answer(context.Background(), &exchange, "what is happening with the voice work", recorder(&spoken)); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(spoken) != 2 || spoken[0].text != "ABC-12 is in progress." || spoken[1].text != "It was updated this morning." {
		t.Fatalf("spoken = %+v", spoken)
	}

	requests := srv.requests()
	if len(requests) != 2 {
		t.Fatalf("%d model calls, want 2", len(requests))
	}
	first := requests[0]
	messages, _ := first["messages"].([]any)
	if len(messages) == 0 || !reflect.DeepEqual(messages[0], map[string]any{"role": "system", "content": systemPrompt}) {
		t.Errorf("the first call does not open with the system prompt: %v", messages)
	}
	if tools, _ := first["tools"].([]any); len(tools) != len(factTools) || first["model"] != "claude-example" {
		t.Errorf("the first call offered %d tools of model %v, want %d of claude-example", len(tools), first["model"], len(factTools))
	}
	messages, _ = requests[1]["messages"].([]any)
	if len(messages) < 2 {
		t.Fatalf("the second call's messages %v end with no tool call and answer", messages)
	}
	assistant, _ := messages[len(messages)-2].(map[string]any)
	answer, _ := messages[len(messages)-1].(map[string]any)
	toolCalls, _ := assistant["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("the model's turn %v carries %d tool calls, want 1", assistant, len(toolCalls))
	}
	toolCall, _ := toolCalls[0].(map[string]any)
	id, _ := toolCall["id"].(string)
	wantFunction := map[string]any{"name": "search_issues", "arguments": `{"query":"voice work"}`}
	if id == "" || !reflect.DeepEqual(toolCall["function"], wantFunction) {
		t.Errorf("the model's tool call went back as %v, want an id and %v", toolCall, wantFunction)
	}
	content, _ := answer["content"].(string)
	if answer["role"] != "tool" || answer["tool_call_id"] != id || !strings.Contains(content, "ABC-12") {
		t.Errorf("the tool's answer went back as %v, want a tool message for %q with the search's issue", answer, id)
	}
}
