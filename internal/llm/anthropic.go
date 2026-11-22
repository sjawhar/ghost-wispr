package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

type anthropicClient struct {
	client   anthropic.Client
	model    string
	provider string // the provider's name, which its errors name
	reported        // the model each answer names
}

func newAnthropicClient(p *providers.Provider, model string) *anthropicClient {
	client := anthropic.NewClient(
		option.WithAPIKey(providers.KeyPlaceholder),
		option.WithBaseURL(p.BaseURL()),
		option.WithHTTPClient(p.HTTPClient(nil)),
	)
	return &anthropicClient{client: client, model: model, provider: p.Name}
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
		return "", fmt.Errorf("%s: empty response content", c.provider)
	}
	return result, nil
}

func (c *anthropicClient) Complete(ctx context.Context, messages []Message) (string, error) {
	systemBlocks, chatMessages := c.convertMessages(messages)
	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: maxAnswerTokens,
		System:    systemBlocks,
		Messages:  chatMessages,
	})
	if err != nil {
		return "", fmt.Errorf("%s completion: %w", c.provider, providererr.From(c.provider, err))
	}
	text, err := c.extractText(resp)
	if err != nil {
		return "", err
	}
	c.record(string(resp.Model))
	return text, nil
}

func (c *anthropicClient) CompleteJSON(ctx context.Context, messages []Message, schema map[string]any) (json.RawMessage, error) {
	systemBlocks, chatMessages := c.convertMessages(messages)
	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: maxAnswerTokens,
		System:    systemBlocks,
		Messages:  chatMessages,
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{
				Schema: schema,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%s json completion: %w", c.provider, providererr.From(c.provider, err))
	}
	text, err := c.extractText(resp)
	if err != nil {
		return nil, err
	}
	c.record(string(resp.Model))
	return json.RawMessage(text), nil
}
