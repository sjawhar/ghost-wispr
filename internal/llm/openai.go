package llm

import (
	"context"
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
