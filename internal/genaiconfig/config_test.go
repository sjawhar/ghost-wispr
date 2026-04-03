package genaiconfig

import (
	"testing"

	"google.golang.org/genai"
)

func TestResolveBackend(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		project string
		want    string
	}{
		{name: "default to gemini without project", want: BackendGemini},
		{name: "default to vertex with project", project: "proj-123", want: BackendVertex},
		{name: "explicit gemini", backend: BackendGemini, project: "proj-123", want: BackendGemini},
		{name: "explicit vertex with project", backend: BackendVertex, project: "proj-123", want: BackendVertex},
		{name: "vertex falls back without project", backend: BackendVertex, want: BackendGemini},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveBackend(tc.backend, tc.project); got != tc.want {
				t.Fatalf("ResolveBackend(%q, %q) = %q, want %q", tc.backend, tc.project, got, tc.want)
			}
		})
	}
}

func TestBuildClientConfigVertex(t *testing.T) {
	cfg, err := BuildClientConfig(&Options{Backend: BackendVertex, Project: "proj-123"})
	if err != nil {
		t.Fatalf("BuildClientConfig returned error: %v", err)
	}
	if cfg.Backend != genai.BackendVertexAI {
		t.Fatalf("expected Vertex backend, got %v", cfg.Backend)
	}
	if cfg.Project != "proj-123" {
		t.Fatalf("expected project proj-123, got %q", cfg.Project)
	}
	if cfg.Location != DefaultLocation {
		t.Fatalf("expected default location %q, got %q", DefaultLocation, cfg.Location)
	}
	if cfg.APIKey != "" {
		t.Fatalf("expected empty API key for Vertex AI, got %q", cfg.APIKey)
	}
}

func TestBuildClientConfigGemini(t *testing.T) {
	cfg, err := BuildClientConfig(&Options{Backend: BackendGemini, APIKey: "test-key", BaseURL: "http://example.test"})
	if err != nil {
		t.Fatalf("BuildClientConfig returned error: %v", err)
	}
	if cfg.Backend != genai.BackendGeminiAPI {
		t.Fatalf("expected Gemini backend, got %v", cfg.Backend)
	}
	if cfg.APIKey != "test-key" {
		t.Fatalf("expected API key to be preserved, got %q", cfg.APIKey)
	}
	if cfg.HTTPOptions.BaseURL != "http://example.test" {
		t.Fatalf("expected base URL override, got %q", cfg.HTTPOptions.BaseURL)
	}
}

func TestBuildClientConfigFallsBackToGeminiWithoutProject(t *testing.T) {
	cfg, err := BuildClientConfig(&Options{Backend: BackendVertex, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("BuildClientConfig returned error: %v", err)
	}
	if cfg.Backend != genai.BackendGeminiAPI {
		t.Fatalf("expected Gemini fallback backend, got %v", cfg.Backend)
	}
}

func TestBuildClientConfigErrorsWithoutCredentials(t *testing.T) {
	_, err := BuildClientConfig(&Options{Backend: BackendGemini})
	if err == nil {
		t.Fatal("expected error when Gemini backend has no API key")
	}
}
