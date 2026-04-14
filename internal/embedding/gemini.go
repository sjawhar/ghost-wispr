package embedding

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
)

type geminiClient struct {
	client *genai.Client
	model  string
}

func newGeminiClient(apiKey, model string, opts *clientOptions) (*geminiClient, error) {
	config, err := genaiconfig.BuildClientConfig(&genaiconfig.Options{
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
	}

	return result, nil
}
