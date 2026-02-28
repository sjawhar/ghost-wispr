package llm

import (
	"strings"
	"testing"
)

func TestParseModel(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantProvider string
		wantModel    string
		wantErr      string
	}{
		{name: "valid", input: "openai/gpt-4o-mini", wantProvider: "openai", wantModel: "gpt-4o-mini"},
		{name: "missing slash", input: "openai", wantErr: "invalid model format"},
		{name: "empty provider", input: "/gpt-4o-mini", wantErr: "invalid model format"},
		{name: "empty model", input: "openai/", wantErr: "invalid model format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, modelName, err := ParseModel(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseModel returned error: %v", err)
			}
			if provider != tt.wantProvider {
				t.Fatalf("expected provider %q, got %q", tt.wantProvider, provider)
			}
			if modelName != tt.wantModel {
				t.Fatalf("expected model %q, got %q", tt.wantModel, modelName)
			}
		})
	}
}

func TestNewClientUnknownProvider(t *testing.T) {
	client, err := NewClient("unknown", "key", "some-model")
	if err == nil {
		t.Fatalf("expected error for unknown provider, got nil")
	}
	if client != nil {
		t.Fatalf("expected nil client, got %#v", client)
	}
	if !strings.Contains(err.Error(), "unknown LLM provider") {
		t.Fatalf("unexpected error: %v", err)
	}
}
