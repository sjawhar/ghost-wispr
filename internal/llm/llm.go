package llm

import (
	"context"
	"fmt"
	"strings"
)

type Message struct {
	Role    string
	Content string
}

type Client interface {
	Complete(ctx context.Context, messages []Message) (string, error)
}

type Option func(*clientOptions)

type clientOptions struct {
	baseURL string
}

func WithBaseURL(url string) Option {
	return func(o *clientOptions) {
		o.baseURL = url
	}
}

func ParseModel(model string) (provider, modelName string, err error) {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid model format %q: expected provider/model_name", model)
	}
	return parts[0], parts[1], nil
}

func NewClient(provider, apiKey, model string, opts ...Option) (Client, error) {
	o := &clientOptions{}
	for _, opt := range opts {
		opt(o)
	}

	switch provider {
	case "openai":
		return newOpenAIClient(apiKey, model, o)
	case "anthropic":
		return newAnthropicClient(apiKey, model, o)
	case "gemini":
		return newGeminiClient(apiKey, model, o)
	default:
		return nil, fmt.Errorf("unknown LLM provider %q: supported providers are openai, anthropic, gemini", provider)
	}
}
