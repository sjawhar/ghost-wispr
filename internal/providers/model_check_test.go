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

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// modelAPIFake is a provider's model list and its model calls: list answers
// GET of listPath, each call answers with the status calls names for its
// model (200 when none), quoting the request in a refusal, and every request
// is kept as "<method> <path>?<query> model=<model>".
type modelAPIFake struct {
	mu       sync.Mutex
	pages    map[string]string // query → list page; "" for the first
	calls    map[string]int
	requests []string
	bodies   []map[string]any
	headers  []http.Header
}

func (f *modelAPIFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	model, _ := body["model"].(string)
	if model == "" && strings.Contains(r.URL.Path, "/models/") {
		model, _, _ = strings.Cut(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ":")
	}
	f.mu.Lock()
	f.requests = append(f.requests, fmt.Sprintf("%s %s?%s model=%s", r.Method, r.URL.Path, r.URL.RawQuery, model))
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, r.Header.Clone())
	page, listed := f.pages[r.URL.RawQuery]
	status := f.calls[model]
	f.mu.Unlock()
	if r.Method == http.MethodGet {
		if !listed {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, page)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"quokkaplanted %s for %s","type":"team_model_access_denied","code":"403"}}`, model, r.Header.Get("Authorization"))
		return
	}
	_, _ = io.WriteString(w, `{}`)
}

func (f *modelAPIFake) sent() ([]string, []map[string]any, []http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), append([]map[string]any(nil), f.bodies...), append([]http.Header(nil), f.headers...)
}

// gatewayChecked checks a gateway at srv, signed in by ID token, whose
// embedding role calls gemini-embedding-example and whose Concierge calls
// claude-example on it.
func gatewayChecked(srv *httptest.Server) Status {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	p, _ := New(&config.Config{
		Providers: map[string]config.Provider{
			"gateway": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
		},
		Embedding: config.Embedding{Model: "gateway/gemini-embedding-example"},
		Concierge: config.Concierge{Model: "gateway/claude-example"},
	}, Options{IDToken: m.IDToken, Now: clk.Now}).Get("gateway")
	return p.Check(context.Background())
}

// TestAGatewayCheckCallsEachModelItsRolesCall fails if a gateway reads as
// working while it lists a model its roles call for no one, or refuses a
// call of it (LiteLLM's 403 team_model_access_denied): only a model list
// that holds each model, and one call of each that the gateway answers, is
// working. It also fails if the calls are not the cheapest of each kind (an
// embedding of one word, a chat completion of one token), are not signed
// in, or if a refusal is quoted.
func TestAGatewayCheckCallsEachModelItsRolesCall(t *testing.T) {
	const both = `{"data":[{"id":"gemini-embedding-example"},{"id":"claude-example"},{"id":"other"}]}`
	t.Run("lists and serves both", func(t *testing.T) {
		fake := &modelAPIFake{pages: map[string]string{"": both}}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		s := gatewayChecked(srv)
		requests, bodies, headers := fake.sent()
		want := []string{"GET /v1/models? model=", "POST /v1/embeddings? model=gemini-embedding-example", "POST /v1/chat/completions? model=claude-example"}
		if !s.OK || !s.Asked || strings.Join(requests, "\n") != strings.Join(want, "\n") {
			t.Fatalf("check %+v after %q; want Works, after the list, an embedding and a chat completion", s, requests)
		}
		if input, _ := bodies[1]["input"].(string); input == "" || strings.Contains(input, " ") {
			t.Errorf("the embedding embeds %q, want one word", bodies[1]["input"])
		}
		if bodies[2]["max_completion_tokens"] != float64(1) || len(bodies[2]["messages"].([]any)) != 1 {
			t.Errorf("the chat completion asks %v, want one message and one token at most", bodies[2])
		}
		for i, h := range headers {
			if !strings.HasPrefix(h.Get("Authorization"), "Bearer eyJ") {
				t.Errorf("request %d carried Authorization %q, want the ID token", i, h.Get("Authorization"))
			}
		}
		if !strings.Contains(s.Message, "gemini-embedding-example") || !strings.Contains(s.Message, "claude-example") {
			t.Errorf("message %q does not name the models the check called", s.Message)
		}
	})
	t.Run("refuses the embedding model", func(t *testing.T) {
		fake := &modelAPIFake{pages: map[string]string{"": both}, calls: map[string]int{"gemini-embedding-example": http.StatusForbidden}}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		s := gatewayChecked(srv)
		host := strings.TrimPrefix(srv.URL, "http://")
		if want := host + " refused gemini-embedding-example for this sign-in (HTTP 403 team_model_access_denied)."; s.OK || !s.Asked || s.Message != want {
			t.Fatalf("check %+v; want Fails, saying %q", s, want)
		}
		if strings.Contains(s.Message, "quokkaplanted") || strings.Contains(s.Message, "eyJ") {
			t.Fatalf("the message quotes the answer or the token: %q", s.Message)
		}
	})
	t.Run("answers the chat model with a server error", func(t *testing.T) {
		fake := &modelAPIFake{pages: map[string]string{"": both}, calls: map[string]int{"claude-example": http.StatusBadGateway}}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		s := gatewayChecked(srv)
		host := strings.TrimPrefix(srv.URL, "http://")
		if want := "Signed in, but " + host + " answered HTTP 502 (team_model_access_denied) to a call of claude-example."; s.OK || !s.Asked || s.Message != want {
			t.Fatalf("check %+v; want Fails, saying %q", s, want)
		}
	})
	t.Run("lists no chat model", func(t *testing.T) {
		fake := &modelAPIFake{pages: map[string]string{"": `{"data":[{"id":"gemini-embedding-example"}]}`}}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		s := gatewayChecked(srv)
		host := strings.TrimPrefix(srv.URL, "http://")
		requests, _, _ := fake.sent()
		if want := host + " accepted the credential but lists no claude-example for it."; s.OK || !s.Asked || s.Message != want || len(requests) != 1 {
			t.Fatalf("check %+v after %q; want Fails, saying %q, with no call made", s, requests, want)
		}
	})
	t.Run("answers no model list", func(t *testing.T) {
		fake := &modelAPIFake{pages: map[string]string{"": `<html>hello</html>`}}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		s := gatewayChecked(srv)
		host := strings.TrimPrefix(srv.URL, "http://")
		if want := host + " accepted the credential but did not answer with a model list."; s.OK || !s.Asked || s.Message != want {
			t.Fatalf("check %+v; want Fails, saying %q", s, want)
		}
	})
}

// TestAnAnthropicCheckReadsEveryPageAndCountsTokens fails if an Anthropic
// API provider's check stops at the first page of the model list, or proves
// a model with anything but a count of a short text's tokens, which costs
// nothing, or sends either without its key and API version.
func TestAnAnthropicCheckReadsEveryPageAndCountsTokens(t *testing.T) {
	fake := &modelAPIFake{pages: map[string]string{
		"limit=1000":                   `{"data":[{"id":"claude-a"}],"has_more":true,"last_id":"claude-a"}`,
		"after_id=claude-a&limit=1000": `{"data":[{"id":"claude-b"}],"has_more":false,"last_id":"claude-b"}`,
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p, _ := New(&config.Config{
		Providers:     map[string]config.Provider{"claude": {API: config.APIAnthropic, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_ANTHROPIC_KEY"}}},
		ProviderKeys:  map[string]string{"EXAMPLE_ANTHROPIC_KEY": "ak-1"},
		Summarization: config.Summarization{Model: "claude/claude-b"},
	}, Options{}).Get("claude")
	s := p.Check(context.Background())
	requests, _, headers := fake.sent()
	want := []string{"GET /v1/models?limit=1000 model=", "GET /v1/models?after_id=claude-a&limit=1000 model=", "POST /v1/messages/count_tokens? model=claude-b"}
	if !s.OK || !s.Asked || strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("check %+v after %q; want Works, after both pages and a token count", s, requests)
	}
	for i, h := range headers {
		if h.Get("X-Api-Key") != "ak-1" || h.Get("Anthropic-Version") == "" {
			t.Errorf("request %d carried x-api-key %q and anthropic-version %q", i, h.Get("X-Api-Key"), h.Get("Anthropic-Version"))
		}
	}
}

// TestAGeminiAPICheckCountsTokensAndEmbedsAWord fails if a Gemini API
// provider's check does not find its models in the model list by their
// names (models/…), or proves a chat model with anything but a token count
// and an embedding model with anything but an embedding of one word.
func TestAGeminiAPICheckCountsTokensAndEmbedsAWord(t *testing.T) {
	fake := &modelAPIFake{pages: map[string]string{"pageSize=1000": `{"models":[{"name":"models/gemini-a"},{"name":"models/gemini-embed"}]}`}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p, _ := New(&config.Config{
		Providers:     map[string]config.Provider{"gemini": {API: config.APIGemini, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_GEMINI_KEY"}}},
		ProviderKeys:  map[string]string{"EXAMPLE_GEMINI_KEY": "gm-1"},
		Summarization: config.Summarization{Model: "gemini/gemini-a"},
		Embedding:     config.Embedding{Model: "gemini/gemini-embed"},
	}, Options{}).Get("gemini")
	s := p.Check(context.Background())
	requests, _, headers := fake.sent()
	want := []string{"GET /v1beta/models?pageSize=1000 model=", "POST /v1beta/models/gemini-a:countTokens? model=gemini-a", "POST /v1beta/models/gemini-embed:embedContent? model=gemini-embed"}
	if !s.OK || !s.Asked || strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("check %+v after %q; want Works, after the list, a token count and an embedding", s, requests)
	}
	for i, h := range headers {
		if h.Get("X-Goog-Api-Key") != "gm-1" {
			t.Errorf("request %d carried x-goog-api-key %q", i, h.Get("X-Goog-Api-Key"))
		}
	}
}

// routeTo is a client whose every request goes to srv, whatever its host,
// as a vendor's public API would answer.
func routeTo(srv *httptest.Server) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})}
}

// TestAVendorsOwnAPIDecidesAnUnlistedAliasByItsCall fails if a model a role
// calls on a vendor's own API (api.anthropic.com, api.openai.com) that its
// list does not name, as an alias such as claude-…-latest is not, reads as
// failing when a call of it is answered, or as working when it is refused.
func TestAVendorsOwnAPIDecidesAnUnlistedAliasByItsCall(t *testing.T) {
	for status, ok := range map[int]bool{0: true, http.StatusNotFound: false} {
		fake := &modelAPIFake{
			pages: map[string]string{"limit=1000": `{"data":[{"id":"claude-example-20991231"}],"has_more":false}`},
			calls: map[string]int{"claude-example-latest": status},
		}
		srv := httptest.NewServer(fake)
		p, _ := New(&config.Config{
			Providers:     map[string]config.Provider{"anthropic": {API: config.APIAnthropic, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_ANTHROPIC_KEY"}}},
			ProviderKeys:  map[string]string{"EXAMPLE_ANTHROPIC_KEY": "ak-1"},
			Summarization: config.Summarization{Model: "anthropic/claude-example-latest"},
		}, Options{Client: routeTo(srv)}).Get("anthropic")
		s := p.Check(context.Background())
		requests, _, _ := fake.sent()
		srv.Close()
		if s.OK != ok || !s.Asked || len(requests) != 2 {
			t.Errorf("a call answered %d: check %+v after %q; want OK %v, decided by the call", status, s, requests, ok)
		}
	}
}

// TestAGatewayWhoseRoleNoCallProvesIsOnlySignedIn fails if a gateway that
// only a text-to-speech role uses reads as working (OK and Asked) after its
// model list alone: Works needs a call of the role's model, and no cheap
// call proves a speech model. With a chat role beside it, the chat call is
// made and the provider is still only Signed in.
func TestAGatewayWhoseRoleNoCallProvesIsOnlySignedIn(t *testing.T) {
	const list = `{"data":[{"id":"tts-example"},{"id":"chat-example"}]}`
	for name, cfg := range map[string]config.Config{
		"speech alone":  {TTS: config.TTS{Model: "gateway/tts-example"}},
		"speech + chat": {TTS: config.TTS{Model: "gateway/tts-example"}, Concierge: config.Concierge{Model: "gateway/chat-example"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &modelAPIFake{pages: map[string]string{"": list}}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			clk := &clock{t: time.Unix(1_800_000_000, 0)}
			m := &minter{clock: clk}
			cfg.Providers = map[string]config.Provider{
				"gateway": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
			}
			p, _ := New(&cfg, Options{IDToken: m.IDToken, Now: clk.Now}).Get("gateway")
			s := p.Check(context.Background())
			requests, _, _ := fake.sent()
			if !s.OK || s.Asked || !strings.Contains(s.Message, "makes no call of tts-example") {
				t.Fatalf("check %+v after %q; want Signed in (OK, not Asked), naming the speech model no call proves", s, requests)
			}
			if cfg.Concierge.Model != "" && !strings.Contains(strings.Join(requests, "\n"), "POST /v1/chat/completions? model=chat-example") {
				t.Fatalf("the chat model was not called: %q", requests)
			}
		})
	}
}

// TestAModelCalledForOneRoleIsNotSaidUncalledForAnother fails if a model
// that serves two roles on a gateway, one with a cheap call (summaries) and
// one without (text-to-speech), is said to be called and not called in one
// message ("answered a call of both-example. The check makes no call of
// both-example."), or if the provider reads as Works: the call proves the
// summaries' use of the model, not the speech's, so the provider stays
// Signed in (not Asked), saying which use no call proves.
func TestAModelCalledForOneRoleIsNotSaidUncalledForAnother(t *testing.T) {
	fake := &modelAPIFake{pages: map[string]string{"": `{"data":[{"id":"both-example"}]}`}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	p, _ := New(&config.Config{
		Providers: map[string]config.Provider{
			"gateway": {API: config.APIOpenAI, URL: srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "example-gateway"}},
		},
		TTS:           config.TTS{Model: "gateway/both-example"},
		Summarization: config.Summarization{Model: "gateway/both-example"},
	}, Options{IDToken: m.IDToken, Now: clk.Now}).Get("gateway")
	s := p.Check(context.Background())
	requests, _, _ := fake.sent()
	host := strings.TrimPrefix(srv.URL, "http://")
	want := "Signed in: " + host + " accepted the credential and answered a call of both-example. No cheap call proves both-example for tts.model."
	if !s.OK || s.Asked || s.Message != want {
		t.Fatalf("check %+v after %q; want Signed in (OK, not Asked), saying %q", s, requests, want)
	}
}
