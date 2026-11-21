package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/tts"
)

// gatewayRequest is what the fake model gateway saw of one request.
type gatewayRequest struct {
	path, key, authorization string
}

type fakeGateway struct {
	mu       sync.Mutex
	requests []gatewayRequest
	url      string
}

func (f *fakeGateway) seen() []gatewayRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gatewayRequest(nil), f.requests...)
}

// startModelGateway starts a fake model gateway, and a fake metadata server
// that answers the box's ID token for the gateway's audience in full format,
// and returns the gateway and that token. The process has no credential file.
func startModelGateway(t *testing.T) (*fakeGateway, string) {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "email": "dev-box@example-project.iam.gserviceaccount.com"})
	token := enc([]byte(`{"alg":"RS256"}`)) + "." + enc(claims) + "." + enc([]byte("sig"))
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		q := r.URL.Query()
		if !strings.HasSuffix(r.URL.Path, "/instance/service-accounts/default/identity") ||
			q.Get("audience") != googleauth.ModelGatewayAudience || q.Get("format") != "full" {
			http.Error(w, "not this token", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(token))
	}))
	t.Cleanup(metadata.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	gatewayCredentialCache = sync.Map{}
	t.Cleanup(func() { gatewayCredentialCache = sync.Map{} })

	f := &fakeGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, gatewayRequest{r.URL.Path, r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization")})
		f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "-tts:generateContent") {
			_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{
				map[string]any{"inlineData": map[string]string{"mimeType": "audio/wav", "data": base64.StdEncoding.EncodeToString(wavOf([]byte{1, 0}))}},
			}}}}})
			return
		}
		if strings.HasSuffix(r.URL.Path, ":generateContent") {
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}`))
			return
		}
		http.Error(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f, token
}

// gatewayConfig is a box's config with the gateway named: Vertex AI settings
// that the gateway route must not read, and no credential file.
func gatewayConfig(gatewayURL string) *config.Config {
	return &config.Config{
		ModelGatewayURL: gatewayURL + "/",
		GenAIBackend:    "vertex",
		GCPProject:      "example-project",
		GCPLocation:     "global",
		TTSProvider:     "gemini",
		TTSModel:        "gemini-3.8-flash-tts",
		Voice:           config.Voice{TTSEngine: config.TTSEngineGemini, TTSModel: "gemini-3.8-flash-tts", TTSLocation: "us"},
	}
}

// TestTheConciergeAndBothGeminiVoicesCallTheModelGateway fails unless, with
// model_gateway_url set, the concierge's model client, voice A's Gemini
// engine and the Gemini TTS provider each send their call to the gateway,
// on Google's own path for the model, with the box's ID token for the
// gateway's audience: the concierge's in x-goog-api-key, which the genai SDK
// sends, and the voices' as their bearer.
func TestTheConciergeAndBothGeminiVoicesCallTheModelGateway(t *testing.T) {
	gateway, token := startModelGateway(t)
	cfg := gatewayConfig(gateway.url)
	ctx := context.Background()

	options, err := conciergeGenAIOptions(ctx, cfg)
	if err != nil {
		t.Fatalf("conciergeGenAIOptions: %v", err)
	}
	clientConfig, err := genaiconfig.BuildClientConfig(options)
	if err != nil {
		t.Fatalf("BuildClientConfig: %v", err)
	}
	client, err := genai.NewClient(ctx, clientConfig)
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	if _, err := client.Models.GenerateContent(ctx, "gemini-3.5-flash-lite", genai.Text("Hello."), nil); err != nil {
		t.Fatalf("the concierge's model call through the gateway: %v", err)
	}

	engine, err := voiceAEngine(ctx, cfg, &voiceAOptions{})
	if err != nil {
		t.Fatalf("voiceAEngine: %v", err)
	}
	_ = engine.Stream(ctx, "Hello.", func([]byte) error { return nil })

	provider, err := newTTSProvider(ctx, cfg)
	if err != nil {
		t.Fatalf("newTTSProvider: %v", err)
	}
	if _, err := provider.Synthesize(ctx, tts.SpeechRequest{Text: "Hello."}); err != nil {
		t.Fatalf("the Gemini TTS provider's call through the gateway: %v", err)
	}

	want := []gatewayRequest{
		{path: "/v1beta/models/gemini-3.5-flash-lite:generateContent", key: token},
		{path: "/v1beta/models/gemini-3.8-flash-tts:streamGenerateContent", authorization: "Bearer " + token},
		{path: "/v1beta/models/gemini-3.8-flash-tts:generateContent", authorization: "Bearer " + token},
	}
	got := gateway.seen()
	if len(got) != len(want) {
		t.Fatalf("the gateway saw %d requests %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestWithNoGatewayTheGeminiCallsStayOnVertexAI fails if a config that names
// no gateway gets gateway options for the concierge: the Pi calls Vertex AI
// as its own account.
func TestWithNoGatewayTheGeminiCallsStayOnVertexAI(t *testing.T) {
	cfg := vertexTestConfig(t)
	options, err := conciergeGenAIOptions(context.Background(), cfg)
	if err != nil {
		t.Fatalf("conciergeGenAIOptions: %v", err)
	}
	if options.GatewayURL != "" || options.GatewayCredentials != nil || options.Credentials == nil {
		t.Fatalf("options with no gateway = %+v, want Vertex AI with its credentials", options)
	}
}
