package embedding

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const envPrefix = "GHOST_WISPR_"

type TaskType string

const (
	TaskTypeDocument TaskType = "RETRIEVAL_DOCUMENT"
	TaskTypeQuery    TaskType = "RETRIEVAL_QUERY"
)

type Client interface {
	Embed(ctx context.Context, texts []string, taskType TaskType) ([][]float32, error)
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

func NewClient(model string, opts ...Option) (Client, error) {
	return newClient(model, opts...)
}

func newClient(model string, opts ...Option) (Client, error) {
	provider, modelName, err := ParseModel(model)
	if err != nil {
		return nil, err
	}

	o := &clientOptions{}
	for _, opt := range opts {
		opt(o)
	}

	switch provider {
	case "openai":
		return newOpenAIClient(os.Getenv(envPrefix+"OPENAI_API_KEY"), modelName, o)
	case "gemini":
		return newGeminiClient(os.Getenv(envPrefix+"GEMINI_API_KEY"), modelName, o)
	default:
		return nil, fmt.Errorf("unknown embedding provider %q: supported providers are openai, gemini", provider)
	}
}
