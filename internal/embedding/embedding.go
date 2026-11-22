package embedding

import (
	"context"
	"fmt"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

const ModelNotReportedByProvider = "not reported by provider"

type TaskType string

const (
	TaskTypeDocument TaskType = "RETRIEVAL_DOCUMENT"
	TaskTypeQuery    TaskType = "RETRIEVAL_QUERY"
)

type Client interface {
	Embed(ctx context.Context, texts []string, taskType TaskType) ([][]float32, error)
}

// NewClient returns the embedding client of model on provider p, by the wire
// API p speaks: the OpenAI API's embeddings, or Gemini's on Vertex AI or the
// Gemini API. Every request goes to p's base URL and carries p's credential.
func NewClient(p *providers.Provider, model string) (Client, error) {
	switch p.API {
	case config.APIOpenAI:
		return newOpenAIClient(p, model), nil
	case config.APIVertex, config.APIGemini:
		return newGeminiClient(p, model)
	default:
		return nil, fmt.Errorf("provider %q speaks the %s API; an embedding client needs openai, vertex or gemini", p.Name, p.API)
	}
}
