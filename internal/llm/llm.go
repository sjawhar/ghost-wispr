package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

const ModelNotReportedByProvider = "not reported by provider"

// maxAnswerTokens caps the tokens of an answer the OpenAI and Anthropic
// clients ask for; Anthropic's Messages API needs a cap, and its client has
// always sent this one. Without a cap, claude-haiku-4-5 through the model
// gateway's OpenAI API wrote one summary of 64,529 characters, ".jl"
// repeated, for 6 min 42 s.
const maxAnswerTokens = 8192

// reported is the model a client's answers name, which a summary records
// as its model.
type reported struct {
	mu    sync.RWMutex
	model string
}

// ReportedModel returns the model the latest successful answer named,
// ModelNotReportedByProvider when it named none, and "" before any answer.
// The configured model never stands in for one: a gateway may serve a
// request under another name.
func (r *reported) ReportedModel() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.model
}

func (r *reported) record(model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = ModelNotReportedByProvider
	}
	r.mu.Lock()
	r.model = model
	r.mu.Unlock()
}

type Message struct {
	Role    string
	Content string
}

type Client interface {
	Complete(ctx context.Context, messages []Message) (string, error)
	CompleteJSON(ctx context.Context, messages []Message, schema map[string]any) (json.RawMessage, error)
}

// NewClient returns the client of model on provider p, by the wire API p
// speaks: the OpenAI API's chat completions, Anthropic's Messages API, or
// Gemini on Vertex AI or the Gemini API. Every request goes to p's base URL
// and carries p's credential.
func NewClient(p *providers.Provider, model string) (Client, error) {
	switch p.API {
	case config.APIOpenAI:
		return newOpenAIClient(p, model), nil
	case config.APIAnthropic:
		return newAnthropicClient(p, model), nil
	case config.APIVertex, config.APIGemini:
		return newGeminiClient(p, model)
	default:
		return nil, fmt.Errorf("provider %q speaks the %s API; a text client needs openai, anthropic, vertex or gemini", p.Name, p.API)
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
