package embedding

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/retry"
)

type openAIEmbedder interface {
	CreateEmbeddings(ctx context.Context, conv openai.EmbeddingRequestConverter) (res openai.EmbeddingResponse, err error)
}

type openaiClient struct {
	client   openAIEmbedder
	model    string
	provider string // the provider's name, which its errors name
	// taskType says whether a request carries the task type, as the
	// top-level field task_type: a Gemini embedding model (its name starts
	// "gemini-") embeds a document and a query each its own way (Vertex AI's
	// taskType), and a model gateway that serves one passes the field on to
	// it. Any other model, such as OpenAI's text-embedding-3-small, has no
	// task type, and OpenAI's own API refuses a field it does not know. The
	// model decides, not the host: one gateway can serve both kinds.
	taskType bool

	mu            sync.RWMutex
	reportedModel string
}

func newOpenAIClient(p *providers.Provider, model string) *openaiClient {
	config := openai.DefaultConfig(providers.KeyPlaceholder)
	config.BaseURL = p.BaseURL()
	config.HTTPClient = retryAfterDoer{next: p.HTTPClient(nil)}
	return &openaiClient{client: openai.NewClientWithConfig(config), model: model, provider: p.Name, taskType: strings.HasPrefix(model, "gemini-")}
}

// retryAfterKey holds, in a call's context, the retryAfterNote its 429
// answer's Retry-After is noted in: go-openai's errors keep no header.
type retryAfterKey struct{}

type retryAfterNote struct {
	after time.Duration
	named bool
}

// retryAfterDoer sends a request, and notes the Retry-After of an answer of
// 429 in the call's retryAfterNote.
type retryAfterDoer struct{ next openai.HTTPDoer }

func (d retryAfterDoer) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.next.Do(req)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		if note, ok := req.Context().Value(retryAfterKey{}).(*retryAfterNote); ok {
			note.after, note.named = retry.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
	}
	return resp, err
}

// Embed embeds each text in a request of its own, one string with float
// vectors: a model gateway's Gemini embedding model takes one input a call,
// and answers a list with one vector of the list as a whole.
func (c *openaiClient) Embed(ctx context.Context, texts []string, taskType TaskType) ([][]float32, error) {
	vectors := make([][]float32, 0, len(texts))
	for i, text := range texts {
		request := openai.EmbeddingRequest{
			Input:          text,
			Model:          openai.EmbeddingModel(c.model),
			EncodingFormat: openai.EmbeddingEncodingFormatFloat,
		}
		if c.taskType && taskType != "" {
			// go-openai merges ExtraBody into the top level of the body.
			request.ExtraBody = map[string]any{"task_type": string(taskType)}
		}
		note := &retryAfterNote{}
		resp, err := c.client.CreateEmbeddings(context.WithValue(ctx, retryAfterKey{}, note), request)
		if err != nil {
			err = providererr.From(c.provider, err)
			if note.named {
				err = retry.WithRetryAfter(err, note.after)
			}
			return nil, fmt.Errorf("%s embedding: %w", c.provider, err)
		}
		if len(resp.Data) != 1 || len(resp.Data[0].Embedding) == 0 {
			return nil, fmt.Errorf("%s: %d embeddings in the answer for input index %d, want one", c.provider, len(resp.Data), i)
		}
		vectors = append(vectors, resp.Data[0].Embedding)
		c.recordReportedModel(string(resp.Model))
	}
	return vectors, nil
}

// ReportedModel returns the model the latest answer with a vector names, or
// ModelNotReportedByProvider when it names none; "" before any. The semantic
// search answer shows it as the provider's model, so the configured model,
// which is no report, never stands in for one. Each stored vector is labelled
// with the configured model (Indexer.SetModel), not this one, so a gateway
// that echoes the model under a prefixed name changes no stored label.
func (c *openaiClient) ReportedModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reportedModel
}

func (c *openaiClient) recordReportedModel(model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = ModelNotReportedByProvider
	}
	c.mu.Lock()
	c.reportedModel = model
	c.mu.Unlock()
}
