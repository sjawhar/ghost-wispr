package providers

import (
	"cmp"
	"fmt"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// KeyPlaceholder fills the API key an SDK demands (genai on the Gemini API,
// go-openai, the Anthropic SDK); the provider's transport (HTTPClient)
// replaces the header that carries it with the provider's credential.
const KeyPlaceholder = "set-by-the-provider-transport"

// GenAIConfig is the genai client config of a Vertex AI or Gemini API
// provider, whose requests carry the provider's credential to its own host.
func (p *Provider) GenAIConfig() (*genai.ClientConfig, error) {
	cc := &genai.ClientConfig{HTTPClient: p.HTTPClient(nil)}
	switch p.API {
	case config.APIVertex:
		cc.Backend = genai.BackendVertexAI
		if p.Auth.Kind == config.AuthAPIKey {
			cc.APIKey = KeyPlaceholder // Vertex AI's express mode
		} else {
			// genai would fail a client with no project itself, but not
			// once it has a base URL: it would call the host with no
			// project in the path.
			if p.Project == "" {
				return nil, fmt.Errorf("provider %q calls Vertex AI and names no project", p.Name)
			}
			cc.Project, cc.Location = p.Project, cmp.Or(p.Location, config.DefaultVertexLocation)
		}
	case config.APIGemini:
		cc.Backend, cc.APIKey = genai.BackendGeminiAPI, KeyPlaceholder
		// v1beta, the version the provider check and the Gemini voice call:
		// genai's default, v1, is a route a model gateway's Google routes
		// (LiteLLM's /v1beta/models/... and /models/...) do not serve.
		cc.HTTPOptions.APIVersion = "v1beta"
	default:
		return nil, fmt.Errorf("provider %q speaks the %s API, which has no genai client", p.Name, p.API)
	}
	// Always the provider's own host: without one genai takes
	// GOOGLE_VERTEX_BASE_URL or GOOGLE_GEMINI_BASE_URL from the environment
	// instead, and names no multi-region's host.
	cc.HTTPOptions.BaseURL = p.BaseURL() + "/"
	return cc, nil
}
