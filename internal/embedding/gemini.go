package embedding

import (
	"context"
	"fmt"

	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"google.golang.org/genai"
)

type geminiClient struct {
	client *genai.Client
	model  string
}

func newGeminiClient(apiKey, model string, opts *clientOptions) (*geminiClient, error) {
	config, err := genaiconfig.BuildClientConfig(genaiconfig.Options{
		Backend:  opts.genai.Backend,
		Project:  opts.genai.Project,
		Location: opts.genai.Location,
		APIKey:   apiKey,
		BaseURL:  opts.baseURL,
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

	contents := make([]*genai.Content, len(texts))
	for i, text := range texts {
		contents[i] = genai.NewContentFromText(text, genai.RoleUser)
	}

	config := &genai.EmbedContentConfig{}
	if taskType != "" {
		config.TaskType = string(taskType)
	}

	resp, err := c.client.Models.EmbedContent(ctx, c.model, contents, config)
	if err != nil {
		return nil, fmt.Errorf("gemini embedding: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("gemini: no embeddings in response")
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("gemini: embeddings count mismatch: got %d, want %d", len(resp.Embeddings), len(texts))
	}

	vectors := make([][]float32, len(resp.Embeddings))
	for i, embedding := range resp.Embeddings {
		if embedding == nil || len(embedding.Values) == 0 {
			return nil, fmt.Errorf("gemini: missing embedding for input index %d", i)
		}
		vectors[i] = embedding.Values
	}

	return vectors, nil
}
