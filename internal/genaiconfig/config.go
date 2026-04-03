package genaiconfig

import (
	"fmt"
	"strings"

	"google.golang.org/genai"
)

const (
	BackendGemini   = "gemini"
	BackendVertex   = "vertex"
	DefaultLocation = "us-central1"
)

type Options struct {
	Backend  string
	Project  string
	Location string
	APIKey   string
	BaseURL  string
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

func CanUse(opts Options) bool {
	switch ResolveBackend(opts.Backend, opts.Project) {
	case BackendVertex:
		return strings.TrimSpace(opts.Project) != ""
	case BackendGemini:
		return strings.TrimSpace(opts.APIKey) != ""
	default:
		return false
	}
}

func BuildClientConfig(opts Options) (*genai.ClientConfig, error) {
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
			Backend:  genai.BackendVertexAI,
			Project:  project,
			Location: location,
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
