package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

type openaiClient struct {
	client *openai.Client
	model  string
}

func newOpenAIClient(apiKey, model string, opts *clientOptions) (*openaiClient, error) {
	config := openai.DefaultConfig(apiKey)
	if opts.baseURL != "" {
		config.BaseURL = opts.baseURL
	}
	return &openaiClient{client: openai.NewClientWithConfig(config), model: model}, nil
}

func (c *openaiClient) Complete(ctx context.Context, messages []Message) (string, error) {
	msgs := make([]openai.ChatCompletionMessage, len(messages))
	for i, m := range messages {
		msgs[i] = openai.ChatCompletionMessage{Role: m.Role, Content: m.Content}
	}

	resp, err := c.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{Model: c.model, Messages: msgs})
	if err != nil {
		return "", fmt.Errorf("openai completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai: no choices in response")
	}

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

	resp, err := c.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: msgs,
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "response",
				Schema: json.RawMessage(schemaBytes),
				Strict: true,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("openai json completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai: no choices in response")
	}

	return json.RawMessage(resp.Choices[0].Message.Content), nil
}
