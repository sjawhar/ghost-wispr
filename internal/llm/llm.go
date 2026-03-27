package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Message struct {
	Role    string
	Content string
}

type Client interface {
	Complete(ctx context.Context, messages []Message) (string, error)
	CompleteJSON(ctx context.Context, messages []Message, schema map[string]any) (json.RawMessage, error)
}

type Option func(*clientOptions)

type clientOptions struct {
	baseURL     string
	gcpProject  string
	gcpLocation string
}

func WithBaseURL(url string) Option {
	return func(o *clientOptions) {
		o.baseURL = url
	}
}

func WithGCPProject(project, location string) Option {
	return func(o *clientOptions) {
		o.gcpProject = project
		o.gcpLocation = location
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

// JSONSchema builds a JSON Schema object for use with CompleteJSON.
// It creates an object schema with the given properties (each mapping name to type string).
// All properties are marked as required.
func JSONSchema(properties map[string]string) map[string]any {
	props := make(map[string]any, len(properties))
	required := make([]string, 0, len(properties))
	for name, typ := range properties {
		props[name] = map[string]any{"type": typ}
		required = append(required, name)
	}
	sort.Strings(required)
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}
