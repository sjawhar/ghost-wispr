package embedding

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

type geminiClient struct {
	client   *genai.Client
	model    string
	provider string // the provider's name, which its errors name

	mu            sync.RWMutex
	reportedModel string
}

// newGeminiClient returns the embedding client of model on a Vertex AI or
// Gemini API provider.
func newGeminiClient(p *providers.Provider, model string) (*geminiClient, error) {
	config, err := p.GenAIConfig()
	if err != nil {
		return nil, err
	}
	client, err := genai.NewClient(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}
	return &geminiClient{client: client, model: model, provider: p.Name}, nil
}

func (c *geminiClient) Embed(ctx context.Context, texts []string, taskType TaskType) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}

	config := &genai.EmbedContentConfig{}
	if taskType != "" {
		config.TaskType = string(taskType)
	}

	// Embed one text at a time — gemini-embedding-2-preview only supports single content per call.
	result := make([][]float32, 0, len(texts))
	for _, text := range texts {
		content := genai.NewContentFromText(text, genai.RoleUser)
		resp, err := c.client.Models.EmbedContent(ctx, c.model, []*genai.Content{content}, config)
		if err != nil {
			return nil, fmt.Errorf("%s embedding: %w", c.provider, providererr.From(c.provider, err))
		}
		if len(resp.Embeddings) == 0 {
			return nil, fmt.Errorf("%s: no embedding in response", c.provider)
		}
		result = append(result, resp.Embeddings[0].Values)
		// Vertex EmbedContent does not echo a model version. Preserve that
		// absence rather than presenting the configured request as a report.
		c.recordReportedModel()
	}

	return result, nil
}

// ReportedModel returns the latest provider model identity. Vertex
// EmbedContent does not return one, so successful calls report that absence.
func (c *geminiClient) ReportedModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reportedModel
}

func (c *geminiClient) recordReportedModel() {
	c.mu.Lock()
	c.reportedModel = ModelNotReportedByProvider
	c.mu.Unlock()
}
