package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// openAIAnswer answers one OpenAI embeddings request with a data entry for
// each of vectors, naming model as the model that embedded them ("" names
// none).
func openAIAnswer(w http.ResponseWriter, model string, vectors ...[]float32) {
	data := make([]map[string]any, 0, len(vectors))
	for i, vector := range vectors {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vector})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list", "model": model, "data": data,
		"usage": map[string]any{"prompt_tokens": 1, "total_tokens": 1},
	})
}

// TestAnOpenAIEmbeddingAnswerOfOtherThanOneVectorFails fails if an OpenAI API
// embedding client takes an answer to its one input that holds no vector, two
// vectors, or one empty vector: the indexer stores each vector as the chunk's
// and search compares the query's with them, so a missing, extra or empty one
// files a chunk under another's vector or none.
func TestAnOpenAIEmbeddingAnswerOfOtherThanOneVectorFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vectors [][]float32
	}{
		{name: "no vector"},
		{name: "two vectors", vectors: [][]float32{{1, 2}, {3, 4}}},
		{name: "an empty vector", vectors: [][]float32{{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				openAIAnswer(w, "gemini-embedding-2", tc.vectors...)
			}))
			defer srv.Close()
			client, err := NewClient(gatewayProvider(t, srv.URL+"/v1"), "gemini-embedding-2")
			if err != nil {
				t.Fatal(err)
			}

			vectors, err := client.Embed(context.Background(), []string{"a chunk"}, TaskTypeDocument)
			if err == nil || vectors != nil {
				t.Fatalf("Embed of an answer with %s = %v, %v; want an error and no vectors", tc.name, vectors, err)
			}
		})
	}
}

// recordingEmbedder keeps the embedding requests it is asked to send and
// answers each with one vector.
type recordingEmbedder struct{ requests []openai.EmbeddingRequest }

func (r *recordingEmbedder) CreateEmbeddings(_ context.Context, conv openai.EmbeddingRequestConverter) (openai.EmbeddingResponse, error) {
	r.requests = append(r.requests, conv.Convert())
	return openai.EmbeddingResponse{Data: []openai.Embedding{{Embedding: []float32{1}}}}, nil
}

// taskTypeSent embeds one query with model on the OpenAI API provider at url
// ("" is OpenAI's own API) and returns the task type its request carries, and
// whether it carries one.
func taskTypeSent(t *testing.T, url, model string) (any, bool) {
	t.Helper()
	c := newOpenAIClient(keyProvider(t, config.APIOpenAI, url), model)
	fake := &recordingEmbedder{}
	c.client = fake
	if _, err := c.Embed(context.Background(), []string{"a chunk"}, TaskTypeQuery); err != nil {
		t.Fatalf("url %q, model %s: Embed: %v", url, model, err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("url %q, model %s: %d requests, want 1", url, model, len(fake.requests))
	}
	taskType, ok := fake.requests[0].ExtraBody["task_type"]
	return taskType, ok
}

// embeddingHosts are OpenAI API base URLs: OpenAI's own, by default and by
// name, and a model gateway's.
var embeddingHosts = []string{"", "https://api.openai.com/v1/", "https://gateway.example.test/v1"}

// TestAGeminiEmbeddingModelIsSentTheTaskType fails if a request for a Gemini
// embedding model (its name starts gemini-), which embeds a document and a
// query each its own way, lacks the task type at any host: the model takes
// it, not the host it is reached at.
func TestAGeminiEmbeddingModelIsSentTheTaskType(t *testing.T) {
	for _, url := range embeddingHosts {
		if taskType, ok := taskTypeSent(t, url, "gemini-embedding-2"); !ok || taskType != "RETRIEVAL_QUERY" {
			t.Errorf("url %q: a gemini-embedding-2 request carries task_type %#v (%v), want RETRIEVAL_QUERY", url, taskType, ok)
		}
	}
}

// TestAnOpenAIEmbeddingModelIsSentNoTaskType fails if a request for a model
// that is not a Gemini embedding model, such as OpenAI's
// text-embedding-3-small, which has no task type, carries task_type at any
// host, a model gateway's included.
func TestAnOpenAIEmbeddingModelIsSentNoTaskType(t *testing.T) {
	for _, url := range embeddingHosts {
		if taskType, ok := taskTypeSent(t, url, "text-embedding-3-small"); ok {
			t.Errorf("url %q: a text-embedding-3-small request carries task_type %#v", url, taskType)
		}
	}
}

// TestAnOpenAIEmbeddingReportsTheModelTheAnswerNames fails if an OpenAI API
// embedding client reports any model but the one its answer names, or, when
// the answer names none, reports other than ModelNotReportedByProvider: the
// semantic search answer shows the reported model as the provider's, so the
// configured model must not pass for one.
func TestAnOpenAIEmbeddingReportsTheModelTheAnswerNames(t *testing.T) {
	for _, tc := range []struct {
		name, answered, want string
	}{
		{name: "named", answered: "gemini-embedding-2-echo", want: "gemini-embedding-2-echo"},
		{name: "not named", answered: "", want: ModelNotReportedByProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				openAIAnswer(w, tc.answered, []float32{1, 2})
			}))
			defer srv.Close()
			client, err := NewClient(gatewayProvider(t, srv.URL+"/v1"), "gemini-embedding-2")
			if err != nil {
				t.Fatal(err)
			}
			reporter, ok := client.(interface{ ReportedModel() string })
			if !ok {
				t.Fatal("the OpenAI API embedding client reports no model")
			}

			if _, err := client.Embed(context.Background(), []string{"a query"}, TaskTypeQuery); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if got := reporter.ReportedModel(); got != tc.want {
				t.Fatalf("ReportedModel() = %q after an answer naming %q, want %q", got, tc.answered, tc.want)
			}
		})
	}
}
