package concierge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// staticToken is a Google access token that never changes.
type staticToken string

func (s staticToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: string(s), Type: "Bearer"}, nil
}

// generatorProvider is entry as the provider "p", signed in by Google
// credentials whose token is "tok-1".
func generatorProvider(t *testing.T, entry *config.Provider) *providers.Provider {
	t.Helper()
	p, err := providers.New(&config.Config{Providers: map[string]config.Provider{"p": *entry}}, providers.Options{
		GoogleCredentials: func(context.Context, string, ...string) (*auth.Credentials, error) {
			return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken("tok-1")}), nil
		},
	}).Get("p")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNewGeneratorRefusesAProviderWithNoClient fails if the Concierge gets a
// model client on a provider whose API it has no client for (Deepgram's), or
// the refusal does not name the provider and its API.
func TestNewGeneratorRefusesAProviderWithNoClient(t *testing.T) {
	p := generatorProvider(t, &config.Provider{API: config.APIDeepgram, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "GHOST_WISPR_DEEPGRAM_API_KEY"}})
	generator, err := NewGenerator(context.Background(), p)
	if err == nil || generator != nil {
		t.Fatalf("NewGenerator on a deepgram provider = %v, %v; want a refusal", generator, err)
	}
	if !strings.Contains(err.Error(), `"p"`) || !strings.Contains(err.Error(), config.APIDeepgram) {
		t.Fatalf("the refusal %q does not name the provider and its API", err)
	}
}

// TestNewGeneratorStreamsFromTheProvider fails if the Concierge's model
// client does not stream the model in the Vertex AI provider's project and
// location, or streams it without the provider's Google access token.
func TestNewGeneratorStreamsFromTheProvider(t *testing.T) {
	var mu sync.Mutex
	var path, authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path, authorization = r.URL.Path, r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}],\"role\":\"model\"}}]}\n\n")
	}))
	defer srv.Close()
	p := generatorProvider(t, &config.Provider{API: config.APIVertex, URL: srv.URL, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}})
	generator, err := NewGenerator(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	for resp, err := range generator.GenerateContentStream(context.Background(), "gemini-x", genai.Text("ping"), nil) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		text.WriteString(resp.Text())
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "/projects/example-project/locations/global/publishers/google/models/gemini-x:streamGenerateContent"; text.String() != "ok" || !strings.HasSuffix(path, want) {
		t.Fatalf("streamed %q from %q; want ok from a path ending %s", text.String(), path, want)
	}
	if authorization != "Bearer tok-1" {
		t.Fatalf("Authorization = %q, want the provider's Google access token", authorization)
	}
}

// TestNewGeneratorNamesTheProviderInItsErrors fails if a Vertex AI provider's
// refusal reaches the Concierge as genai's error, whose text quotes the
// provider's message, or is not named as the config names the provider, with
// its HTTP status and code.
func TestNewGeneratorNamesTheProviderInItsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error": {"code": 429, "message": "no quota for pineapple", "status": "RESOURCE_EXHAUSTED"}}`)
	}))
	defer srv.Close()
	p := generatorProvider(t, &config.Provider{API: config.APIVertex, URL: srv.URL, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}})
	generator, err := NewGenerator(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for _, err := range generator.GenerateContentStream(context.Background(), "gemini-x", genai.Text("what about pineapple"), nil) {
		if err != nil {
			streamErr = err
			break
		}
	}
	var reduced *providererr.Error
	if !errors.As(streamErr, &reduced) || reduced.Provider != "p" {
		t.Fatalf("error %v (%T), want a providererr.Error of the provider p", streamErr, streamErr)
	}
	if text := streamErr.Error(); !strings.Contains(text, "HTTP 429") || !strings.Contains(text, "RESOURCE_EXHAUSTED") || strings.Contains(text, "pineapple") {
		t.Fatalf("error %q, want its status and code and none of the provider's message", text)
	}
}

// TestAnErrorNoProviderSentNamesNoVendor fails if an error no provider sent
// (a model client's own, or the ear rewrite's empty answer) reaches the
// Concierge's error or log under a vendor's name: it runs on Gemini, OpenAI
// and a model gateway's models alike.
func TestAnErrorNoProviderSentNamesNoVendor(t *testing.T) {
	namesNoVendor := func(what, text string) {
		t.Helper()
		for _, vendor := range []string{"gemini", "openai", "vertex"} {
			if strings.Contains(text, vendor) {
				t.Errorf("%s names %s:\n%s", what, vendor, text)
			}
		}
	}
	r := newRig(t)
	logs := r.captureLogs()
	r.model.errs = []error{errors.New("the client broke")}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "x", recorder(&spoken))
	if err == nil {
		t.Fatal("Answer hid the model client's failure")
	}
	namesNoVendor("Answer's error", err.Error())
	namesNoVendor("the log of the failed answer", logs.String())

	r = newRig(t)
	logs = r.captureLogs()
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.ask(t, "agent-7", "q", "")
	r.model.turns = [][]*genai.GenerateContentResponse{{}, {}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Done."}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.concierge.Answers():
	case <-time.After(5 * time.Second):
		t.Fatal("no pending answer")
	}
	if !strings.Contains(logs.String(), "the model gave no text") {
		t.Errorf("the log does not say the rewrite was empty:\n%s", logs)
	}
	namesNoVendor("the log of the empty rewrite", logs.String())
}
