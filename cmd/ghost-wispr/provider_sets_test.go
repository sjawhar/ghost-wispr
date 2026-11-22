package main

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestAModelThatChangesRoleBuildsANewProviderSet fails if a settings change
// that moves a model from one role to another on the same provider (a
// summary model becoming the embedding model) keeps the provider Set, whose
// check would still call the model as the old role does (a chat completion,
// not an embedding), or if the same config builds a second Set.
func TestAModelThatChangesRoleBuildsANewProviderSet(t *testing.T) {
	sets := &providerSets{}
	gateway := map[string]config.Provider{
		"gateway": {API: config.APIOpenAI, URL: "https://gateway.example.test/v1", Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
	}
	asSummary := &config.Config{Providers: gateway, Summarization: config.Summarization{Model: "gateway/shared-model"}}
	asEmbedding := &config.Config{Providers: gateway, Embedding: config.Embedding{Model: "gateway/shared-model"}}

	first := sets.of(asSummary)
	if again := sets.of(asSummary); again != first {
		t.Fatal("the same config built a second provider Set")
	}
	if moved := sets.of(asEmbedding); moved == first {
		t.Fatal("moving gateway/shared-model from summaries to embeddings kept the Set whose check calls it as a chat model")
	}
}
