package genaiconfig

import (
	"fmt"
	"net/http"
	"strings"

	"cloud.google.com/go/auth"

	"google.golang.org/genai"
)

const (
	BackendGemini   = "gemini"
	BackendVertex   = "vertex"
	DefaultLocation = "us-central1"
)

type Options struct {
	Backend     string
	Project     string
	Location    string
	APIKey      string
	BaseURL     string
	Credentials *auth.Credentials
	// GatewayURL, when set, sends every call to the model gateway instead of
	// the backend above: the Gemini API's own paths under GatewayURL, which
	// the gateway serves by calling Vertex AI itself. Backend, Project,
	// Location, APIKey and BaseURL are not read.
	GatewayURL string
	// GatewayCredentials give the gateway's ID token, sent on each request
	// in x-goog-api-key, the header the gateway reads as the caller's key.
	GatewayCredentials *auth.Credentials
}

func NormalizeBackend(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case BackendGemini:
		return BackendGemini
	case BackendVertex:
		return BackendVertex
	default:
		return ""
	}
}

func DefaultBackend(project string) string {
	if strings.TrimSpace(project) != "" {
		return BackendVertex
	}
	return BackendGemini
}

func ResolveBackend(raw, project string) string {
	backend := NormalizeBackend(raw)
	if backend == "" {
		backend = DefaultBackend(project)
	}
	if backend == BackendVertex && strings.TrimSpace(project) == "" {
		return BackendGemini
	}
	return backend
}

func CanUse(opts *Options) bool {
	if strings.TrimSpace(opts.GatewayURL) != "" {
		return opts.GatewayCredentials != nil
	}
	switch ResolveBackend(opts.Backend, opts.Project) {
	case BackendVertex:
		return strings.TrimSpace(opts.Project) != ""
	case BackendGemini:
		return strings.TrimSpace(opts.APIKey) != ""
	default:
		return false
	}
}

// gatewayPlaceholderKey is the key a gateway client is built with: no
// credential, since gatewayKey replaces it with the ID token on each request.
const gatewayPlaceholderKey = "replaced-by-the-id-token"

func BuildClientConfig(opts *Options) (*genai.ClientConfig, error) {
	if gateway := strings.TrimRight(strings.TrimSpace(opts.GatewayURL), "/"); gateway != "" {
		if opts.GatewayCredentials == nil {
			return nil, fmt.Errorf("the model gateway needs credentials for its ID token")
		}
		// The SDK needs a key to take the Gemini API's shape; gatewayKey
		// replaces it on each request with the credentials' current token.
		// The gateway serves Google's routes under /v1beta (and /models), not
		// /v1, so the version is v1beta whatever the SDK's default.
		return &genai.ClientConfig{
			APIKey:      gatewayPlaceholderKey,
			Backend:     genai.BackendGeminiAPI,
			HTTPOptions: genai.HTTPOptions{BaseURL: gateway, APIVersion: "v1beta"},
			HTTPClient:  &http.Client{Transport: &gatewayKey{credentials: opts.GatewayCredentials, next: http.DefaultTransport}},
		}, nil
	}
	project := strings.TrimSpace(opts.Project)
	apiKey := strings.TrimSpace(opts.APIKey)
	location := strings.TrimSpace(opts.Location)
	if location == "" {
		location = DefaultLocation
	}

	var cfg *genai.ClientConfig
	switch ResolveBackend(opts.Backend, project) {
	case BackendVertex:
		if project == "" {
			return nil, fmt.Errorf("vertex ai backend requires a GCP project")
		}
		cfg = &genai.ClientConfig{
			Backend:     genai.BackendVertexAI,
			Project:     project,
			Location:    location,
			Credentials: opts.Credentials,
		}
	case BackendGemini:
		if apiKey == "" {
			return nil, fmt.Errorf("gemini api key is required")
		}
		cfg = &genai.ClientConfig{
			APIKey:  apiKey,
			Backend: genai.BackendGeminiAPI,
		}
	default:
		return nil, fmt.Errorf("unsupported genai backend %q", opts.Backend)
	}

	if strings.TrimSpace(opts.BaseURL) != "" {
		cfg.HTTPOptions.BaseURL = strings.TrimSpace(opts.BaseURL)
	}

	return cfg, nil
}

// gatewayKey puts the credentials' current token in each request's
// x-goog-api-key, so a token renewed by the credentials' cache reaches the
// next request. It never adds an Authorization header.
type gatewayKey struct {
	credentials *auth.Credentials
	next        http.RoundTripper
}

func (k *gatewayKey) RoundTrip(r *http.Request) (*http.Response, error) {
	token, err := k.credentials.Token(r.Context())
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, fmt.Errorf("the model gateway's ID token: %w", err)
	}
	r = r.Clone(r.Context())
	r.Header.Set("x-goog-api-key", token.Value)
	r.Header.Del("Authorization")
	return k.next.RoundTrip(r)
}
