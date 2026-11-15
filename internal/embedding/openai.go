package embedding

import (
	"context"
	"fmt"
	"sync"

	openai "github.com/sashabaranov/go-openai"
)

type openAIEmbedder interface {
	CreateEmbeddings(ctx context.Context, conv openai.EmbeddingRequestConverter) (res openai.EmbeddingResponse, err error)
}

type openaiClient struct {
	client openAIEmbedder
	model  string

	mu            sync.RWMutex
	reportedModel string
}

func newOpenAIClient(apiKey, model string, opts *clientOptions) (*openaiClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("openai api key is required")
	}

	config := openai.DefaultConfig(apiKey)
	if opts.baseURL != "" {
		config.BaseURL = opts.baseURL
	}

	return &openaiClient{client: openai.NewClientWithConfig(config), model: model}, nil
}

func (c *openaiClient) Embed(ctx context.Context, texts []string, _ TaskType) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}

	resp, err := c.client.CreateEmbeddings(ctx, openai.EmbeddingRequestStrings{
		Input: texts,
		Model: openai.EmbeddingModel(c.model),
	})
	if err != nil {
		return nil, fmt.Errorf("openai embedding: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("openai: no embeddings in response")
	}

	vectors := make([][]float32, len(texts))
	for _, item := range resp.Data {
		if item.Index < 0 || item.Index >= len(texts) {
			return nil, fmt.Errorf("openai: invalid embedding index %d", item.Index)
		}
		vectors[item.Index] = item.Embedding
	}

	for i, v := range vectors {
		if len(v) == 0 {
			return nil, fmt.Errorf("openai: missing embedding for input index %d", i)
		}
	}
	c.mu.Lock()
	c.reportedModel = c.model
	c.mu.Unlock()

	return vectors, nil
}

// ReportedModel returns the resolved request model after a successful
// embedding response. OpenAI's embedding response does not echo one.
func (c *openaiClient) ReportedModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reportedModel
}
