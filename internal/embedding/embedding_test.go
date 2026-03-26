package embedding

import (
	"context"
	"strings"
	"testing"
)

type mockClient struct {
	vectors [][]float32
}

func (m *mockClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if len(texts) > len(m.vectors) {
		return nil, context.DeadlineExceeded
	}

	out := make([][]float32, len(texts))
	copy(out, m.vectors[:len(texts)])
	return out, nil
}

func TestEmbedReturnsVectors(t *testing.T) {
	client := &mockClient{vectors: [][]float32{{0.1, 0.2, 0.3}, {0.4, 0.5, 0.6}}}

	vectors, err := client.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}

	if len(vectors) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vectors))
	}
	if len(vectors[0]) != 3 || len(vectors[1]) != 3 {
		t.Fatalf("expected vectors to have dimension 3, got %d and %d", len(vectors[0]), len(vectors[1]))
	}
}

func TestUnknownProvider(t *testing.T) {
	client, err := NewClient("unknown/text-embedding-model")
	if err == nil {
		t.Fatalf("expected error for unknown provider, got nil")
	}
	if client != nil {
		t.Fatalf("expected nil client, got %#v", client)
	}
	if !strings.Contains(err.Error(), "unknown embedding provider") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBatchEmbedding(t *testing.T) {
	client := &mockClient{vectors: [][]float32{{0.1}, {0.2}, {0.3}}}

	vectors, err := client.Embed(context.Background(), []string{"one", "two", "three"})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}

	if len(vectors) != 3 {
		t.Fatalf("expected 3 vectors for batch input, got %d", len(vectors))
	}
}
