package concierge

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// openaiGenerator streams the Concierge's model from a provider of the OpenAI
// API's chat completions: OpenAI itself, or a model gateway, whose OpenAI API
// serves Claude, OpenAI and Gemini models alike. It translates genai's request
// to chat completions, and the streamed chunks back to genai's answers, so the
// Concierge's turn runs as it does on Gemini:
//
//   - the system instruction is the system message; a user content's text is
//     a user message, and a model content's text and function calls are one
//     assistant message with tool calls;
//   - each function response is a tool message, for the tool call of its id;
//   - the model's thoughts and thought signatures stay out: the OpenAI API
//     takes neither back;
//   - function declarations are function tools, with their JSON Schema
//     (ParametersJsonSchema); function calling mode NONE is tool_choice none;
//   - the thinking level is not sent: the OpenAI API's models that do not
//     reason refuse reasoning_effort;
//   - the temperature is the temperature; a JSON answer (ResponseMIMEType
//     application/json) is response_format json_object, or json_schema with
//     its ResponseJsonSchema, not strict, so a schema strict mode would
//     refuse still runs;
//   - each text delta comes back as a text part as it arrives, and each tool
//     call, once the model finished (a finish_reason) and its arguments are
//     whole, as one function call, under the provider's id or, when it sends
//     none, one of its own. A stream that ends before the model finished is
//     a failure, and its tool calls do not come back.
//
// Anything else a request sets has no place in chat completions, and is
// refused by name (untranslated) rather than dropped. Every error is a
// *providererr.Error: the provider's own words, which can quote the room's,
// are never kept.
type openaiGenerator struct {
	client   *openai.Client
	provider string // the provider's name, which every error names
}

func newOpenAIGenerator(p *providers.Provider) *openaiGenerator {
	config := openai.DefaultConfig(providers.KeyPlaceholder)
	config.BaseURL = p.BaseURL()
	config.HTTPClient = p.HTTPClient(nil)
	return &openaiGenerator{client: openai.NewClientWithConfig(config), provider: p.Name}
}

func (g *openaiGenerator) GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		request, err := chatRequest(model, contents, config)
		if err != nil {
			// No request was sent: the failure is the generator's own.
			yield(nil, &providererr.Error{Provider: g.provider, Failure: err.Error()})
			return
		}
		stream, err := g.client.CreateChatCompletionStream(ctx, request)
		if err != nil {
			yield(nil, providererr.From(g.provider, err))
			return
		}
		defer func() { _ = stream.Close() }()
		var calls streamedCalls
		finished := false
		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				yield(nil, providererr.From(g.provider, err))
				return
			}
			for i := range chunk.Choices {
				choice := &chunk.Choices[i]
				if choice.Index != 0 {
					continue
				}
				if choice.Delta.Content != "" && !yield(modelAnswer(&genai.Part{Text: choice.Delta.Content}), nil) {
					return
				}
				calls.add(choice.Delta.ToolCalls)
				finished = finished || choice.FinishReason != ""
			}
		}
		if !finished {
			yield(nil, &providererr.Error{Provider: g.provider, Failure: "the stream ended before the model finished its answer"})
			return
		}
		parts, err := calls.parts()
		switch {
		case err != nil:
			yield(nil, &providererr.Error{Provider: g.provider, Failure: err.Error()})
		case len(parts) > 0:
			yield(modelAnswer(parts...), nil)
		}
	}
}

// modelAnswer is a streamed answer of the model that holds parts.
func modelAnswer(parts ...*genai.Part) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Role: genai.RoleModel, Parts: parts}}}}
}

// streamedCall is a tool call as its pieces arrive: its index in the stream,
// its id and name, and its arguments so far.
type streamedCall struct {
	index    int
	id, name string
	args     strings.Builder
}

// streamedCalls are a stream's tool calls, in the order they began.
type streamedCalls []*streamedCall

// add adds each piece of a delta's tool calls to its call: the one at the
// piece's index or, from a provider that sends none, at its place in the
// delta. The id and name are the first sent; the arguments come in pieces.
func (c *streamedCalls) add(pieces []openai.ToolCall) {
	for i, piece := range pieces {
		index := i
		if piece.Index != nil {
			index = *piece.Index
		}
		call := c.at(index)
		if call.id == "" {
			call.id = piece.ID
		}
		if call.name == "" {
			call.name = piece.Function.Name
		}
		call.args.WriteString(piece.Function.Arguments)
	}
}

func (c *streamedCalls) at(index int) *streamedCall {
	for _, call := range *c {
		if call.index == index {
			return call
		}
	}
	call := &streamedCall{index: index}
	*c = append(*c, call)
	return call
}

// parts are the calls as function call parts, each with its whole arguments.
// A call the provider gave no id gets one, which the Concierge sends back
// with the call and its answer.
func (c streamedCalls) parts() ([]*genai.Part, error) {
	parts := make([]*genai.Part, 0, len(c))
	for _, call := range c {
		if call.name == "" {
			return nil, errors.New("the model called a tool and named none")
		}
		var args map[string]any
		if raw := strings.TrimSpace(call.args.String()); raw != "" {
			// The arguments can hold the room's words: the error says only whose.
			if json.Unmarshal([]byte(raw), &args) != nil {
				return nil, fmt.Errorf("the model's arguments to %s are no JSON object", toolLabel(call.name))
			}
		}
		id := call.id
		if id == "" {
			id = "call_" + rand.Text()
		}
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: call.name, Args: args}})
	}
	return parts, nil
}

// chatRequest is genai's request as the OpenAI API's chat completions take
// it. It names the first setting that has no translation, and holds none of
// the request's words.
func chatRequest(model string, contents []*genai.Content, config *genai.GenerateContentConfig) (openai.ChatCompletionRequest, error) {
	request := openai.ChatCompletionRequest{Model: model}
	if config == nil {
		config = &genai.GenerateContentConfig{}
	}
	if field := untranslated(config, "SystemInstruction", "Tools", "ToolConfig", "ThinkingConfig", "Temperature", "ResponseMIMEType", "ResponseJsonSchema"); field != "" {
		return request, fmt.Errorf("the request sets %s, which the OpenAI API has no place for", field)
	}
	if config.Temperature != nil {
		// go-openai leaves out a temperature of 0 (omitempty), which the API
		// would take as its default, 1; its README's way to send 0 is this.
		request.Temperature = max(*config.Temperature, math.SmallestNonzeroFloat32)
	}
	format, err := responseFormat(config)
	if err != nil {
		return request, err
	}
	request.ResponseFormat = format
	if config.SystemInstruction != nil {
		var system strings.Builder
		for _, part := range config.SystemInstruction.Parts {
			if field := untranslated(part, "Text"); field != "" {
				return request, fmt.Errorf("the system instruction sets %s, which the OpenAI API has no place for", field)
			}
			system.WriteString(part.Text)
		}
		if system.Len() > 0 {
			request.Messages = append(request.Messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: system.String()})
		}
	}
	for _, content := range contents {
		messages, err := chatMessages(content)
		if err != nil {
			return request, err
		}
		request.Messages = append(request.Messages, messages...)
	}
	for _, tool := range config.Tools {
		if field := untranslated(tool, "FunctionDeclarations"); field != "" {
			return request, fmt.Errorf("a tool sets %s, which the OpenAI API has no place for", field)
		}
		for _, declaration := range tool.FunctionDeclarations {
			if field := untranslated(declaration, "Name", "Description", "ParametersJsonSchema"); field != "" {
				return request, fmt.Errorf("function %q sets %s, which the OpenAI API has no place for", declaration.Name, field)
			}
			parameters := declaration.ParametersJsonSchema
			if parameters == nil {
				parameters = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			request.Tools = append(request.Tools, openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
				Name: declaration.Name, Description: declaration.Description, Parameters: parameters,
			}})
		}
	}
	if config.ToolConfig != nil {
		choice, err := toolChoice(config.ToolConfig)
		if err != nil {
			return request, err
		}
		// The OpenAI API refuses a tool_choice with no tools.
		if choice != "" && len(request.Tools) > 0 {
			request.ToolChoice = choice
		}
	}
	return request, nil
}

// responseFormat is the OpenAI API's response_format for genai's answer
// shape: json_object for a JSON answer, json_schema with its schema when it
// names one, and none for text.
func responseFormat(config *genai.GenerateContentConfig) (*openai.ChatCompletionResponseFormat, error) {
	switch config.ResponseMIMEType {
	case "", "text/plain":
		if config.ResponseJsonSchema != nil {
			return nil, errors.New("the request sets ResponseJsonSchema for an answer that is not JSON")
		}
		return nil, nil
	case "application/json":
	default:
		return nil, errors.New("the request sets ResponseMIMEType to a type the OpenAI API cannot answer in")
	}
	if config.ResponseJsonSchema == nil {
		return &openai.ChatCompletionResponseFormat{Type: openai.ChatCompletionResponseFormatTypeJSONObject}, nil
	}
	schema, err := json.Marshal(config.ResponseJsonSchema)
	if err != nil {
		return nil, errors.New("the request's ResponseJsonSchema is not JSON")
	}
	return &openai.ChatCompletionResponseFormat{
		Type:       openai.ChatCompletionResponseFormatTypeJSONSchema,
		JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{Name: "response", Schema: json.RawMessage(schema)},
	}, nil
}

// toolChoice is the OpenAI API's tool_choice for genai's function calling
// mode: none for NONE, and the API's own default, auto, for AUTO or none set.
func toolChoice(config *genai.ToolConfig) (string, error) {
	if field := untranslated(config, "FunctionCallingConfig"); field != "" {
		return "", fmt.Errorf("the tool config sets %s, which the OpenAI API has no place for", field)
	}
	calling := config.FunctionCallingConfig
	if calling == nil {
		return "", nil
	}
	if field := untranslated(calling, "Mode"); field != "" {
		return "", fmt.Errorf("the function calling config sets %s, which the OpenAI API has no place for", field)
	}
	switch calling.Mode {
	case "", genai.FunctionCallingConfigModeUnspecified, genai.FunctionCallingConfigModeAuto:
		return "", nil
	case genai.FunctionCallingConfigModeNone:
		return "none", nil
	}
	return "", fmt.Errorf("function calling mode %s has no translation for the OpenAI API", calling.Mode)
}

// chatMessages are a content's messages: a model content's text and
// function calls as one assistant message; a user content's function
// responses as tool messages, then its text as a user message.
func chatMessages(content *genai.Content) ([]openai.ChatCompletionMessage, error) {
	var text strings.Builder
	var calls []openai.ToolCall
	var answers []openai.ChatCompletionMessage
	for _, part := range content.Parts {
		if part == nil {
			continue
		}
		if field := untranslated(part, "Text", "Thought", "ThoughtSignature", "FunctionCall", "FunctionResponse"); field != "" {
			return nil, fmt.Errorf("a part sets %s, which the OpenAI API has no place for", field)
		}
		switch {
		case part.Thought:
		case part.FunctionCall != nil:
			call, err := toolCall(part.FunctionCall)
			if err != nil {
				return nil, err
			}
			calls = append(calls, call)
		case part.FunctionResponse != nil:
			answer, err := toolMessage(part.FunctionResponse)
			if err != nil {
				return nil, err
			}
			answers = append(answers, answer)
		default:
			text.WriteString(part.Text)
		}
	}
	switch content.Role {
	case genai.RoleModel:
		if len(answers) > 0 {
			return nil, errors.New("a model content holds a function response, which only a user content holds")
		}
		if text.Len() == 0 && len(calls) == 0 {
			return nil, nil
		}
		return []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleAssistant, Content: text.String(), ToolCalls: calls}}, nil
	case genai.RoleUser, "":
		if len(calls) > 0 {
			return nil, errors.New("a user content holds a function call, which only a model content holds")
		}
		if text.Len() > 0 {
			answers = append(answers, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: text.String()})
		}
		return answers, nil
	}
	return nil, fmt.Errorf("a content's role %q has no translation for the OpenAI API", content.Role)
}

// toolCall is a model's function call as an assistant message's tool call.
// Its id is what pairs it with its answer's tool message.
func toolCall(call *genai.FunctionCall) (openai.ToolCall, error) {
	if field := untranslated(call, "ID", "Name", "Args"); field != "" {
		return openai.ToolCall{}, fmt.Errorf("the function call of %s sets %s, which the OpenAI API has no place for", toolLabel(call.Name), field)
	}
	if call.ID == "" {
		return openai.ToolCall{}, fmt.Errorf("the function call of %s has no id, which pairs a tool call with its answer", toolLabel(call.Name))
	}
	args, err := jsonObject(call.Args)
	if err != nil {
		return openai.ToolCall{}, fmt.Errorf("the arguments of the function call of %s: %w", toolLabel(call.Name), err)
	}
	return openai.ToolCall{ID: call.ID, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: call.Name, Arguments: args}}, nil
}

// toolMessage is a function response as the tool message that answers the
// tool call of its id.
func toolMessage(response *genai.FunctionResponse) (openai.ChatCompletionMessage, error) {
	if field := untranslated(response, "ID", "Name", "Response"); field != "" {
		return openai.ChatCompletionMessage{}, fmt.Errorf("the function response of %s sets %s, which the OpenAI API has no place for", toolLabel(response.Name), field)
	}
	if response.ID == "" {
		return openai.ChatCompletionMessage{}, fmt.Errorf("the function response of %s has no id, which pairs a tool call with its answer", toolLabel(response.Name))
	}
	body, err := jsonObject(response.Response)
	if err != nil {
		return openai.ChatCompletionMessage{}, fmt.Errorf("the function response of %s: %w", toolLabel(response.Name), err)
	}
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: response.ID, Content: body}, nil
}

// toolLabel names a tool in an error: by its name when that is a plain token
// (providererr.Token), as the Concierge's tools are, else only as a tool. The
// model writes the name of a tool it calls, and it can write the room's words.
func toolLabel(name string) string {
	if providererr.Token(name) == "" {
		return "a tool"
	}
	return strconv.Quote(name)
}

// jsonObject is m as a JSON object, {} when it is nil. Its error holds none
// of m's values, which can hold the room's words.
func jsonObject(m map[string]any) (string, error) {
	if m == nil {
		return "{}", nil
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", errors.New("they are no JSON object")
	}
	return string(encoded), nil
}

// untranslated names the first exported field of *v, a struct, that is set
// and is not one of translated: a setting chat completions would not get.
func untranslated(v any, translated ...string) string {
	value := reflect.ValueOf(v).Elem()
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if field.IsExported() && !value.Field(i).IsZero() && !slices.Contains(translated, field.Name) {
			return field.Name
		}
	}
	return ""
}
