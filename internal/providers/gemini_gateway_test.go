package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// seenRequest is one request a fake server was sent: its method, path and
// query, and every header that can carry a credential or a placeholder.
type seenRequest struct {
	Call    string
	Headers map[string]string
}

// requestLog keeps every request a fake server is sent.
type requestLog struct {
	mu   sync.Mutex
	seen []seenRequest
}

func (l *requestLog) record(r *http.Request) {
	headers := map[string]string{}
	for _, name := range authHeaders {
		if v := r.Header.Get(name); v != "" {
			headers[name] = v
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, seenRequest{Call: r.Method + " " + r.URL.RequestURI(), Headers: headers})
}

func (l *requestLog) all() []seenRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]seenRequest(nil), l.seen...)
}

// geminiAnswer is a generateContent answer, and a streamGenerateContent
// event, of the text "ok".
const geminiAnswer = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"modelVersion":"gemini-example"}`

// TestNoCredentialReachesAnotherPortOfTheProvidersHost fails if a provider's
// credential, or the placeholder key an SDK sends, reaches another origin a
// provider redirects a call to: the provider's own hostname at another port,
// where Go's client copies x-goog-api-key and keeps Authorization, and
// another hostname. It covers each client a provider builds: genai's (the
// GenAI client config, a Gemini API key in x-goog-api-key and an ID token in
// Authorization), the provider's HTTP client (a Deepgram key, an ID token),
// and its check.
func TestNoCredentialReachesAnotherPortOfTheProvidersHost(t *testing.T) {
	var other requestLog
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiAnswer)
	}))
	defer away.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(away.URL, "http://"), ":")
	for _, target := range []string{"http://127.0.0.1:" + port, "http://localhost:" + port} {
		t.Run(target, func(t *testing.T) {
			other.mu.Lock()
			other.seen = nil
			other.mu.Unlock()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			}))
			defer provider.Close()
			clk := &clock{t: time.Unix(1_800_000_000, 0)}
			set := New(&config.Config{
				Providers: map[string]config.Provider{
					"gemini":    {API: config.APIGemini, URL: provider.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_GEMINI_KEY"}},
					"gemini-gw": {API: config.APIGemini, URL: provider.URL, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
					"deepgram":  {API: config.APIDeepgram, URL: provider.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_DEEPGRAM_KEY"}},
					"openai-gw": {API: config.APIOpenAI, URL: provider.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
				},
				ProviderKeys: map[string]string{"EXAMPLE_GEMINI_KEY": "gm-1", "EXAMPLE_DEEPGRAM_KEY": "dg-1"},
			}, Options{IDToken: (&minter{clock: clk}).IDToken, Now: clk.Now})
			for _, name := range []string{"gemini", "gemini-gw"} {
				p, _ := set.Get(name)
				cc, err := p.GenAIConfig()
				if err != nil {
					t.Fatal(err)
				}
				client, err := genai.NewClient(context.Background(), cc)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.Models.GenerateContent(context.Background(), "gemini-example", genai.Text("hi"), nil); err == nil || !strings.Contains(err.Error(), "redirected") {
					t.Errorf("%s: genai's call redirected to %s = %v; want it stopped, saying the provider redirected it away", name, target, err)
				}
			}
			for _, name := range []string{"deepgram", "openai-gw"} {
				p, _ := set.Get(name)
				if resp, err := p.HTTPClient(nil).Get(provider.URL + "/v1/models"); err == nil {
					_ = resp.Body.Close()
					t.Errorf("%s: the client's call redirected to %s answered HTTP %d; want it stopped", name, target, resp.StatusCode)
				}
			}
			for _, name := range []string{"gemini", "gemini-gw", "deepgram", "openai-gw"} {
				p, _ := set.Get(name)
				if s := p.Check(context.Background()); s.OK {
					t.Errorf("%s: the check redirected to %s: %+v; want it stopped", name, target, s)
				}
			}
			if seen := other.all(); len(seen) > 0 {
				t.Fatalf("the redirect's target %s was sent %+v; want nothing", target, seen)
			}
		})
	}
}

// TestAGeminiAPIProviderAtAGatewaySignsInWithItsIDToken fails if a provider
// {api: gemini, url: <a gateway's root>, auth: google_id_token}, as a model
// gateway serves Google's routes, does not send genai's calls to
// <url>/v1beta/models/<model>:generateContent and :streamGenerateContent
// (alt=sse) with Authorization: Bearer <its ID token>, or sends genai's
// placeholder key in x-goog-api-key beside it.
func TestAGeminiAPIProviderAtAGatewaySignsInWithItsIDToken(t *testing.T) {
	var gateway requestLog
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.record(r)
		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\n", geminiAnswer)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiAnswer)
	}))
	defer srv.Close()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	set := New(&config.Config{Providers: map[string]config.Provider{
		"gateway-gemini": {API: config.APIGemini, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "model-gateway-example"}},
	}}, Options{IDToken: m.IDToken, Now: clk.Now})
	p, _ := set.Get("gateway-gemini")
	token, err := p.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cc, err := p.GenAIConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := genai.NewClient(context.Background(), cc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Models.GenerateContent(context.Background(), "gemini-3.5-flash-lite", genai.Text("hi"), nil); err != nil {
		t.Fatalf("generateContent: %v", err)
	}
	for _, err := range client.Models.GenerateContentStream(context.Background(), "gemini-3.5-flash-lite", genai.Text("hi"), nil) {
		if err != nil {
			t.Fatalf("streamGenerateContent: %v", err)
		}
	}
	want := []seenRequest{
		{Call: "POST /v1beta/models/gemini-3.5-flash-lite:generateContent", Headers: map[string]string{"Authorization": "Bearer " + token}},
		{Call: "POST /v1beta/models/gemini-3.5-flash-lite:streamGenerateContent?alt=sse", Headers: map[string]string{"Authorization": "Bearer " + token}},
	}
	got := gateway.all()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the gateway was sent\n%+v\nwant\n%+v", got, want)
	}
	if len(m.audiences) == 0 || m.audiences[0] != "model-gateway-example" {
		t.Fatalf("the ID token was minted for %q; want the provider's audience", m.audiences)
	}
}

// googleRoutesGateway is a model gateway's Google routes as LiteLLM serves
// them: no model list (GET /v1beta/models answers 404), no countTokens, and
// generateContent of each model, refused with 403 for the models in refuse.
// It keeps each request it is sent, and each generateContent body.
type googleRoutesGateway struct {
	requestLog
	refuse map[string]bool
	mu     sync.Mutex
	bodies []string
}

func (g *googleRoutesGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.record(r)
	body, _ := io.ReadAll(r.Body)
	model, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1beta/models/"), ":")
	if r.Method != http.MethodPost || method != "generateContent" {
		http.NotFound(w, r)
		return
	}
	g.mu.Lock()
	g.bodies = append(g.bodies, string(body))
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if g.refuse[model] {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":403,"message":"team_model_access_denied: secret words","status":"PERMISSION_DENIED"}}`)
		return
	}
	_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"o"}]},"finishReason":"MAX_TOKENS"}]}`)
}

// TestAGeminiAPICheckAtAGatewayCallsEachChatModel fails if the check of a
// Gemini API provider at a model gateway, whose Google routes serve no model
// list (404) and no token count, does not prove each chat model its roles
// call with one generateContent of at most one output token, signed in with
// its ID token as a bearer token and no x-goog-api-key: a gateway that
// answers those calls would read as failing while its calls work. It also
// fails if a model the gateway refuses (403) does not fail the check naming
// the model and the refusal's status and code alone, or if a speech model,
// which no cheap call proves, reads as Works.
func TestAGeminiAPICheckAtAGatewayCallsEachChatModel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tts       string
		refuse    map[string]bool
		ok, asked bool
		message   []string
		calls     []string
	}{
		{name: "chat models", ok: true, asked: true,
			message: []string{"answered a call of gemini-sum-example, gemini-chat-example"},
			calls:   []string{"POST /v1beta/models/gemini-sum-example:generateContent", "POST /v1beta/models/gemini-chat-example:generateContent"}},
		{name: "and a speech model", tts: "gemini-tts-example", ok: true,
			message: []string{"answered a call of gemini-sum-example, gemini-chat-example", "makes no call of gemini-tts-example"},
			calls:   []string{"POST /v1beta/models/gemini-sum-example:generateContent", "POST /v1beta/models/gemini-chat-example:generateContent"}},
		{name: "a refused model", refuse: map[string]bool{"gemini-chat-example": true}, asked: true,
			message: []string{"refused gemini-chat-example for this sign-in (HTTP 403 PERMISSION_DENIED)"},
			calls:   []string{"POST /v1beta/models/gemini-sum-example:generateContent", "POST /v1beta/models/gemini-chat-example:generateContent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &googleRoutesGateway{refuse: tc.refuse}
			srv := httptest.NewServer(gateway)
			defer srv.Close()
			clk := &clock{t: time.Unix(1_800_000_000, 0)}
			cfg := &config.Config{
				Providers: map[string]config.Provider{
					"gw": {API: config.APIGemini, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "model-gateway-example"}},
				},
				Concierge:     config.Concierge{Model: "gw/gemini-chat-example"},
				Summarization: config.Summarization{Model: "gw/gemini-sum-example"},
			}
			if tc.tts != "" {
				cfg.TTS = config.TTS{Model: "gw/" + tc.tts}
			}
			p, _ := New(cfg, Options{IDToken: (&minter{clock: clk}).IDToken, Now: clk.Now}).Get("gw")
			token, err := p.Credential(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			s := p.Check(context.Background())
			if s.OK != tc.ok || s.Asked != tc.asked {
				t.Errorf("check %+v; want OK %t, asked %t", s, tc.ok, tc.asked)
			}
			for _, want := range tc.message {
				if !strings.Contains(s.Message, want) {
					t.Errorf("check message %q; want it to say %q", s.Message, want)
				}
			}
			if strings.Contains(s.Message, "secret words") {
				t.Errorf("check message %q quotes the gateway's answer", s.Message)
			}
			var calls []string
			for _, seen := range gateway.all() {
				if seen.Headers["Authorization"] != "Bearer "+token || len(seen.Headers) != 1 {
					t.Errorf("%s was sent %v; want the ID token as a bearer token and no key header", seen.Call, seen.Headers)
				}
				if strings.Contains(seen.Call, ":generateContent") {
					calls = append(calls, seen.Call)
				}
			}
			if strings.Join(calls, "\n") != strings.Join(tc.calls, "\n") {
				t.Errorf("the check called %q; want %q", calls, tc.calls)
			}
			gateway.mu.Lock()
			defer gateway.mu.Unlock()
			for _, body := range gateway.bodies {
				var call struct {
					Contents         []map[string]any `json:"contents"`
					GenerationConfig struct {
						MaxOutputTokens int `json:"maxOutputTokens"`
					} `json:"generationConfig"`
				}
				if err := json.Unmarshal([]byte(body), &call); err != nil || call.GenerationConfig.MaxOutputTokens != 1 || len(call.Contents) != 1 {
					t.Errorf("a generateContent body %s; want one user text and generationConfig.maxOutputTokens 1", body)
				}
			}
		})
	}
}
