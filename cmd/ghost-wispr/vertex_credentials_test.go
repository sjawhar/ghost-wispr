package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/googleauthtest"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

const vertexTestToken = "x509-" + "federated-" + "token"

type vertexRequestRecorder struct {
	mu            sync.Mutex
	authorization []string
}

func (r *vertexRequestRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		r.mu.Lock()
		r.authorization = append(r.authorization, request.Header.Get("Authorization"))
		r.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func vertexTestConfig(t *testing.T) *config.Config {
	t.Helper()
	credentialPath := googleauthtest.X509CredentialFile(t, vertexTestToken)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	t.Setenv("GHOST_WISPR_EMBEDDING_BACKEND", "")
	t.Setenv("GHOST_WISPR_GENAI_BACKEND", "vertex")
	return &config.Config{GoogleCredentialsFile: credentialPath, GenAIBackend: "vertex", GCPProject: "theorem-agent-infra", GCPLocation: "global"}
}

func (r *vertexRequestRecorder) assertAuthorized(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, authorization := range r.authorization {
		if authorization == "Bearer "+vertexTestToken {
			return
		}
	}
	t.Fatalf("Vertex requests carried %q, want the configured X.509 credential token", r.authorization)
}

func TestVertexUsesConfiguredCredentials(t *testing.T) {
	vertexCredentialCache = sync.Map{}
	t.Cleanup(func() { vertexCredentialCache = sync.Map{} })
	cfg := vertexTestConfig(t)

	summaryRecorder := &vertexRequestRecorder{}
	summaryServer := summaryRecorder.server(t)
	summaryOptions := genAIOptions(cfg, "")
	if err := addVertexCredentials(context.Background(), cfg, summaryOptions); err != nil {
		t.Fatal(err)
	}
	summaryClient, err := llm.NewClient("gemini", "", "gemini-x", llm.WithBaseURL(summaryServer.URL), llm.WithGenAIConfig(llm.GenAIConfig{Backend: summaryOptions.Backend, Project: summaryOptions.Project, Location: summaryOptions.Location, Credentials: summaryOptions.Credentials}))
	if err == nil {
		_, _ = summaryClient.Complete(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
	}
	summaryRecorder.assertAuthorized(t)

	embeddingRecorder := &vertexRequestRecorder{}
	embeddingServer := embeddingRecorder.server(t)
	embeddingOptions := &genaiconfig.Options{Backend: "vertex", Project: cfg.GCPProject, Location: "us-central1"}
	if err := addVertexCredentials(context.Background(), cfg, embeddingOptions); err != nil {
		t.Fatal(err)
	}
	embeddingClient, err := embedding.NewClient("gemini/gemini-embedding-2", embedding.WithBaseURL(embeddingServer.URL), embedding.WithGenAIConfig(embedding.GenAIConfig{Backend: embeddingOptions.Backend, Project: embeddingOptions.Project, Location: embeddingOptions.Location, Credentials: embeddingOptions.Credentials}))
	if err == nil {
		_, _ = embeddingClient.Embed(context.Background(), []string{"hi"}, embedding.TaskTypeDocument)
	}
	embeddingRecorder.assertAuthorized(t)

	batchRecorder := &vertexRequestRecorder{}
	batchServer := batchRecorder.server(t)
	batchOptions := genAIOptions(cfg, "")
	if err := addVertexCredentials(context.Background(), cfg, batchOptions); err != nil {
		t.Fatal(err)
	}
	batchOptions.BaseURL = batchServer.URL
	batchConfig, err := genaiconfig.BuildClientConfig(batchOptions)
	if err == nil {
		batchClient, clientErr := genai.NewClient(context.Background(), batchConfig)
		if clientErr == nil {
			_, _ = batchClient.Batches.Get(context.Background(), "projects/p/locations/global/batchPredictionJobs/1", nil)
		}
	}
	batchRecorder.assertAuthorized(t)
}

func TestVertexCredentialCacheTracksStoreUpdates(t *testing.T) {
	vertexCredentialCache = sync.Map{}
	t.Cleanup(func() { vertexCredentialCache = sync.Map{} })
	firstCfg := vertexTestConfig(t)
	store, _, err := config.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) {
		cfg.GoogleCredentialsFile = firstCfg.GoogleCredentialsFile
		cfg.GenAIBackend = "vertex"
		cfg.GCPProject = firstCfg.GCPProject
		cfg.GCPLocation = firstCfg.GCPLocation
	}); err != nil {
		t.Fatal(err)
	}
	firstCurrent := store.Get()
	firstOptions := genAIOptions(&firstCurrent, "")
	if err := addVertexCredentials(context.Background(), &firstCurrent, firstOptions); err != nil {
		t.Fatal(err)
	}
	reusedOptions := genAIOptions(&firstCurrent, "")
	if err := addVertexCredentials(context.Background(), &firstCurrent, reusedOptions); err != nil {
		t.Fatal(err)
	}
	if firstOptions.Credentials != reusedOptions.Credentials {
		t.Fatal("credential cache did not reuse credentials before the path update")
	}

	secondToken := "second-" + "federated-" + "token"
	secondPath := googleauthtest.X509CredentialFile(t, secondToken)
	if err := store.Update(func(cfg *config.Config) { cfg.GoogleCredentialsFile = secondPath }); err != nil {
		t.Fatal(err)
	}
	secondCfg := store.Get()
	secondOptions := genAIOptions(&secondCfg, "")
	if err := addVertexCredentials(context.Background(), &secondCfg, secondOptions); err != nil {
		t.Fatal(err)
	}
	if firstOptions.Credentials == secondOptions.Credentials {
		t.Fatal("credential cache did not change after google_credentials_file update")
	}

	recorder := &vertexRequestRecorder{}
	server := recorder.server(t)
	secondOptions.BaseURL = server.URL
	clientConfig, err := genaiconfig.BuildClientConfig(secondOptions)
	if err != nil {
		t.Fatal(err)
	}
	client, err := genai.NewClient(context.Background(), clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Batches.Get(context.Background(), "projects/p/locations/global/batchPredictionJobs/1", nil)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.authorization) == 0 || recorder.authorization[0] != "Bearer "+secondToken {
		t.Fatalf("updated Vertex request authorization = %q, want second credential token", recorder.authorization)
	}
}
