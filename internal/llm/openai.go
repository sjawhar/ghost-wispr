package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/sjawhar/ghost-wispr/internal/jsonschema"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

type openaiClient struct {
	client   *openai.Client
	model    string
	provider string // the provider's name, which its errors name
	reported        // the model each answer names
}

func newOpenAIClient(p *providers.Provider, model string) *openaiClient {
	config := openai.DefaultConfig(providers.KeyPlaceholder)
	config.BaseURL = p.BaseURL()
	config.HTTPClient = p.HTTPClient(nil)
	return &openaiClient{client: openai.NewClientWithConfig(config), model: model, provider: p.Name}
}

func (c *openaiClient) Complete(ctx context.Context, messages []Message) (string, error) {
	msgs := make([]openai.ChatCompletionMessage, len(messages))
	for i, m := range messages {
		msgs[i] = openai.ChatCompletionMessage{Role: m.Role, Content: m.Content}
	}

	// max_completion_tokens, which OpenAI's reasoning models take and
	// refuse max_tokens for; LiteLLM maps it for each vendor.
	resp, err := c.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{Model: c.model, Messages: msgs, MaxCompletionTokens: maxAnswerTokens})
	if err != nil {
		return "", fmt.Errorf("%s completion: %w", c.provider, providererr.From(c.provider, err))
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("%s: no choices in response", c.provider)
	}

	c.record(resp.Model)
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

func (c *openaiClient) CompleteJSON(ctx context.Context, messages []Message, schema map[string]any) (json.RawMessage, error) {
	msgs := make([]openai.ChatCompletionMessage, len(messages))
	for i, m := range messages {
		msgs[i] = openai.ChatCompletionMessage{Role: m.Role, Content: m.Content}
	}

	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal json schema: %w", err)
	}

	// Strict output only where strict mode takes the schema
	// (jsonschema.Strict): a schema with a map in it is refused with HTTP
	// 400 on OpenAI and on a gateway in front of it. Without strict, the
	// model follows the schema as best it can.
	resp, err := c.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:               c.model,
		Messages:            msgs,
		MaxCompletionTokens: maxAnswerTokens,
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "response",
				Schema: json.RawMessage(schemaBytes),
				Strict: jsonschema.Strict(schema),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%s json completion: %w", c.provider, providererr.From(c.provider, err))
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("%s: no choices in response", c.provider)
	}

	c.record(resp.Model)
	return json.RawMessage(resp.Choices[0].Message.Content), nil
}
