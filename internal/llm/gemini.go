package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

type geminiClient struct {
	client   *genai.Client
	model    string
	provider string            // the provider's name, which its errors name
	labels   map[string]string // the provider's GenerateLabels, which each call carries
	// reported is each answer's ModelVersion. Vertex may omit it; the
	// model resolved into the request is then the only model identity the
	// API provides, and the report says the answer named none.
	reported
}

// newGeminiClient returns the client of model on a Vertex AI or Gemini API
// provider.
func newGeminiClient(p *providers.Provider, model string) (*geminiClient, error) {
	config, err := p.GenAIConfig()
	if err != nil {
		return nil, err
	}
	client, err := genai.NewClient(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}
	return &geminiClient{client: client, model: model, provider: p.Name, labels: p.GenerateLabels()}, nil
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

	config := &genai.GenerateContentConfig{SystemInstruction: systemInstruction, Labels: c.labels}
	result, err := c.client.Models.GenerateContent(ctx, c.model, contents, config)
	if err != nil {
		return "", fmt.Errorf("%s completion: %w", c.provider, providererr.From(c.provider, err))
	}

	text := strings.TrimSpace(result.Text())
	if text == "" {
		return "", fmt.Errorf("%s: empty response text", c.provider)
	}
	c.record(result.ModelVersion)
	return text, nil
}

func (c *geminiClient) CompleteJSON(ctx context.Context, messages []Message, schema map[string]any) (json.RawMessage, error) {
	systemInstruction, contents := convertGeminiMessages(messages)

	hasUserMessage := false
	for _, m := range messages {
		if m.Role == "user" {
			hasUserMessage = true
			break
		}
	}
	if !hasUserMessage {
		return nil, fmt.Errorf("gemini: no user message provided")
	}

	config := &genai.GenerateContentConfig{
		SystemInstruction:  systemInstruction,
		ResponseMIMEType:   "application/json",
		ResponseJsonSchema: schema,
		Labels:             c.labels,
	}
	result, err := c.client.Models.GenerateContent(ctx, c.model, contents, config)
	if err != nil {
		return nil, fmt.Errorf("%s json completion: %w", c.provider, providererr.From(c.provider, err))
	}

	text := strings.TrimSpace(result.Text())
	if text == "" {
		return nil, fmt.Errorf("%s: empty response text", c.provider)
	}
	c.record(result.ModelVersion)
	return json.RawMessage(text), nil
}
