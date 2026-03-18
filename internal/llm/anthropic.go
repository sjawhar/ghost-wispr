package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type anthropicClient struct {
	client anthropic.Client
	model  string
}

func newAnthropicClient(apiKey, model string, opts *clientOptions) (*anthropicClient, error) {
	clientOpts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if opts.baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.baseURL))
	}

	return &anthropicClient{client: anthropic.NewClient(clientOpts...), model: model}, nil
}

func (c *anthropicClient) convertMessages(messages []Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam) {
	var systemBlocks []anthropic.TextBlockParam
	var chatMessages []anthropic.MessageParam
	for _, m := range messages {
		switch m.Role {
		case "system":
			systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: m.Content})
		case "user":
			chatMessages = append(chatMessages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case "assistant":
			chatMessages = append(chatMessages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		}
	}
	return systemBlocks, chatMessages
}

func (c *anthropicClient) extractText(resp *anthropic.Message) (string, error) {
	var b strings.Builder
	for i := range resp.Content {
		block := &resp.Content[i]
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	result := strings.TrimSpace(b.String())
	if result == "" {
		return "", fmt.Errorf("anthropic: empty response content")
	}
	return result, nil
}

func (c *anthropicClient) Complete(ctx context.Context, messages []Message) (string, error) {
	systemBlocks, chatMessages := c.convertMessages(messages)
	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 8192,
		System:    systemBlocks,
		Messages:  chatMessages,
	})
	if err != nil {
		return "", fmt.Errorf("anthropic completion: %w", err)
	}
	return c.extractText(resp)
}

func (c *anthropicClient) CompleteJSON(ctx context.Context, messages []Message, schema map[string]any) (json.RawMessage, error) {
	systemBlocks, chatMessages := c.convertMessages(messages)
	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 8192,
		System:    systemBlocks,
		Messages:  chatMessages,
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{
				Schema: schema,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic json completion: %w", err)
	}
	text, err := c.extractText(resp)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(text), nil
}
