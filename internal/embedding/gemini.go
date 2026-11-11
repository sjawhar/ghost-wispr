package embedding

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
)

type geminiClient struct {
	client *genai.Client
	model  string

	mu            sync.RWMutex
	reportedModel string
}

func newGeminiClient(apiKey, model string, opts *clientOptions) (*geminiClient, error) {
	config, err := genaiconfig.BuildClientConfig(&genaiconfig.Options{
		Backend:     opts.genai.Backend,
		Project:     opts.genai.Project,
		Location:    opts.genai.Location,
		APIKey:      apiKey,
		BaseURL:     opts.baseURL,
		Credentials: opts.genai.Credentials,
	})
	if err != nil {
		return nil, err
	}

	client, err := genai.NewClient(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}

	return &geminiClient{client: client, model: model}, nil
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
			return nil, fmt.Errorf("gemini embedding: %w", err)
		}
		if len(resp.Embeddings) == 0 {
			return nil, fmt.Errorf("gemini: no embedding in response")
		}
		result = append(result, resp.Embeddings[0].Values)
		// EmbedContent does not return a model version. After a successful
		// provider call, retain the resolved model sent to Vertex rather than
		// making the API handler read application configuration.
		c.recordReportedModel()
	}

	return result, nil
}

// ReportedModel returns the model used by the latest successful embedding
// request. Vertex EmbedContent does not echo a model version.
func (c *geminiClient) ReportedModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reportedModel
}

func (c *geminiClient) recordReportedModel() {
	c.mu.Lock()
	c.reportedModel = c.model
	c.mu.Unlock()
}
