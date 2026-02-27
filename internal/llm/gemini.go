package llm

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

type geminiClient struct {
	client *genai.Client
	model  string
}

func newGeminiClient(apiKey, model string, opts *clientOptions) (*geminiClient, error) {
	ctx := context.Background()
	config := &genai.ClientConfig{APIKey: apiKey, Backend: genai.BackendGeminiAPI}
	if opts.baseURL != "" {
		config.HTTPOptions.BaseURL = opts.baseURL
	}

	client, err := genai.NewClient(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}

	return &geminiClient{client: client, model: model}, nil
}

func convertGeminiMessages(messages []Message) (*genai.Content, []*genai.Content) {
	var systemInstruction *genai.Content
	var contents []*genai.Content

	for _, m := range messages {
		switch m.Role {
		case "system":
			systemInstruction = &genai.Content{Parts: []*genai.Part{{Text: m.Content}}}
		case "user":
			contents = append(contents, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: m.Content}}})
		case "assistant":
			contents = append(contents, &genai.Content{Role: "model", Parts: []*genai.Part{{Text: m.Content}}})
		}
	}

	return systemInstruction, contents
}

func (c *geminiClient) Complete(ctx context.Context, messages []Message) (string, error) {
	systemInstruction, contents := convertGeminiMessages(messages)

	hasUserMessage := false
	for _, m := range messages {
		if m.Role == "user" {
			hasUserMessage = true
			break
		}
	}
	if !hasUserMessage {
		return "", fmt.Errorf("gemini: no user message provided")
	}

	config := &genai.GenerateContentConfig{SystemInstruction: systemInstruction}
	result, err := c.client.Models.GenerateContent(ctx, c.model, contents, config)
	if err != nil {
		return "", fmt.Errorf("gemini completion: %w", err)
	}

	text := strings.TrimSpace(result.Text())
	if text == "" {
		return "", fmt.Errorf("gemini: empty response text")
	}
	return text, nil
}
