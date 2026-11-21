package genaiconfig

import (
	"fmt"
	"net/http"
	"net/url"
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
		origin, err := url.Parse(gateway)
		if err != nil || origin.Scheme == "" || origin.Host == "" {
			return nil, fmt.Errorf("the model gateway's URL %q is not absolute", gateway)
		}
		return &genai.ClientConfig{
			APIKey:      gatewayPlaceholderKey,
			Backend:     genai.BackendGeminiAPI,
			HTTPOptions: genai.HTTPOptions{BaseURL: gateway, APIVersion: "v1beta"},
			HTTPClient: &http.Client{Transport: &gatewayKey{
				scheme: origin.Scheme, host: origin.Host, credentials: opts.GatewayCredentials, next: http.DefaultTransport,
			}},
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

// gatewayKey puts the credentials' current token in the x-goog-api-key of
// each request to the gateway's own scheme and host, so a token renewed by
// the credentials' cache reaches the next request. A request anywhere else,
// as a redirect can make, carries no key at all. It never adds an
// Authorization header.
type gatewayKey struct {
	scheme, host string
	credentials  *auth.Credentials
	next         http.RoundTripper
}

func (k *gatewayKey) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Del("x-goog-api-key")
	r.Header.Del("Authorization")
	if r.URL.Scheme != k.scheme || r.URL.Host != k.host {
		return k.next.RoundTrip(r)
	}
	token, err := k.credentials.Token(r.Context())
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, fmt.Errorf("the model gateway's ID token: %w", err)
	}
	r.Header.Set("x-goog-api-key", token.Value)
	return k.next.RoundTrip(r)
}
