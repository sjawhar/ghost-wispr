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
	"sync/atomic"
	"testing"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// staticGoogle is a GoogleCredentials option whose credentials hold gt-1.
func staticGoogle(context.Context, string, ...string) (*auth.Credentials, error) {
	return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: auth.NewCachedTokenProvider(googleTokens{new(atomic.Int32)}, nil)}), nil
}

// countTokensFake is Vertex AI's countTokens: it answers each model with its
// status in answers (200 when none), quoting what it was sent in a refusal,
// and keeps each request.
type countTokensFake struct {
	answers map[string]int
	mu      sync.Mutex
	asked   []string // "<method> <path> <Authorization>"
	texts   []string
}

func (f *countTokensFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Contents []struct {
			Role  string
			Parts []struct{ Text string }
		}
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.asked = append(f.asked, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	if len(body.Contents) == 1 && body.Contents[0].Role == "user" && len(body.Contents[0].Parts) == 1 {
		f.texts = append(f.texts, body.Contents[0].Parts[0].Text)
	}
	f.mu.Unlock()
	model, _, _ := strings.Cut(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ":")
	if code := f.answers[model]; code != 0 {
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"no model %s for %s"}}`, code, model, r.Header.Get("Authorization"))
		return
	}
	_, _ = io.WriteString(w, `{"totalTokens":1}`)
}

func (f *countTokensFake) requests() (asked, texts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...), append([]string(nil), f.texts...)
}

// vertexChecked checks a Vertex AI provider at srv whose roles call the given
// summarization, embedding and concierge models on it.
func vertexChecked(srv *httptest.Server, summarization, embedding, concierge string) Status {
	ref := func(model string) string {
		if model == "" {
			return ""
		}
		return "vertex/" + model
	}
	p, _ := New(&config.Config{
		Providers: map[string]config.Provider{
			"vertex": {API: config.APIVertex, URL: srv.URL, Project: "example-project", Location: "us", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
		},
		Summarization: config.Summarization{Model: ref(summarization)},
		Embedding:     config.Embedding{Model: ref(embedding)},
		Concierge:     config.Concierge{Model: ref(concierge)},
	}, Options{GoogleCredentials: staticGoogle}).Get("vertex")
	return p.Check(context.Background())
}

// TestAVertexCheckCountsTokensOfAModelItServes fails if a Vertex AI
// provider's check stops at minting a Google token instead of asking the
// provider, with that token, to count the tokens of a model a role calls on
// it (a call the predict permission alone admits, which costs nothing); if a
// model the provider does not count (an embedding model's 400) reads as a
// failure of the provider, or leaves it reading Works once another model was
// counted, as GPT-6.1 Sol's review of 743fe02 found: that model is unproved,
// so the provider is only Signed in, naming it; if every model counted does
// not read Works; if a model or location it does not serve (its 404) reads as
// working; or if a refusal reads as working or is quoted.
func TestAVertexCheckCountsTokensOfAModelItServes(t *testing.T) {
	const path = "/v1/projects/example-project/locations/us/publishers/google/models/"
	counting := &countTokensFake{}
	srvAll := httptest.NewServer(counting)
	defer srvAll.Close()
	if s := vertexChecked(srvAll, "gemini-example", "", "gemini-example-lite"); !s.OK || !s.Asked {
		t.Fatalf("a provider that counted every model its roles call: %+v; want Works (OK and asked)", s)
	}

	fake := &countTokensFake{answers: map[string]int{"gemini-embedding-example": http.StatusBadRequest}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s := vertexChecked(srv, "", "gemini-embedding-example", "gemini-example-lite")
	asked, texts := fake.requests()
	want := []string{
		"POST " + path + "gemini-embedding-example:countTokens Bearer gt-1",
		"POST " + path + "gemini-example-lite:countTokens Bearer gt-1",
	}
	if strings.Join(asked, "\n") != strings.Join(want, "\n") || len(texts) != 2 || texts[0] == "" {
		t.Fatalf("the check asked %q with texts %q; want a count of each model in the roles' order with the token, one short user text each", asked, texts)
	}
	if !s.OK || s.Asked || !strings.Contains(s.Message, "gemini-example-lite") || !strings.Contains(s.Message, "proves no call of gemini-embedding-example") {
		t.Fatalf("a provider that counted one model and not the embedding model: %+v; want Signed in (OK, not asked), naming the model counted and the one no call proves", s)
	}

	unserved := &countTokensFake{answers: map[string]int{"gemini-unknown-example": http.StatusNotFound}}
	srv5 := httptest.NewServer(unserved)
	defer srv5.Close()
	s = vertexChecked(srv5, "gemini-unknown-example", "", "gemini-example-lite")
	if want := strings.TrimPrefix(srv5.URL, "http://") + " answered HTTP 404 to a count of gemini-unknown-example: check the location and the model."; s.OK || !s.Asked || s.Message != want {
		t.Fatalf("a model or location the provider does not serve: %+v; want not OK, asked, saying %q", s, want)
	}

	refusing := &countTokensFake{answers: map[string]int{"gemini-example": http.StatusForbidden}}
	srv2 := httptest.NewServer(refusing)
	defer srv2.Close()
	s = vertexChecked(srv2, "gemini-example", "", "")
	host := strings.TrimPrefix(srv2.URL, "http://")
	if want := host + " refused the request (HTTP 403) to a count of gemini-example: check the sign-in, and the project and location."; s.OK || !s.Asked || s.Message != want || strings.Contains(s.Message, "gt-1") || strings.Contains(s.Message, "no model") {
		t.Fatalf("a provider that refuses: %+v; want not OK, asked, saying %q and quoting nothing", s, want)
	}

	uncounted := &countTokensFake{answers: map[string]int{"gemini-embedding-example": http.StatusBadRequest}}
	srv3 := httptest.NewServer(uncounted)
	defer srv3.Close()
	s = vertexChecked(srv3, "", "gemini-embedding-example", "")
	if !s.OK || s.Asked || !strings.Contains(s.Message, "Google issued an access token") {
		t.Fatalf("a provider that counts none of its models: %+v; want OK by the token minted, not asked", s)
	}

	unused := &countTokensFake{}
	srv4 := httptest.NewServer(unused)
	defer srv4.Close()
	s = vertexChecked(srv4, "", "", "")
	if asked, _ := unused.requests(); !s.OK || s.Asked || len(asked) != 0 {
		t.Fatalf("a provider no role calls: %+v after %q; want OK by the token minted, asking nothing", s, asked)
	}
}

// TestAVertexCheckCountsEveryModelItsRolesCall fails if a Vertex AI
// provider reads Works once the first model its roles call is counted,
// without counting the others: a refusal of the Concierge's model, after the
// summary model was counted, would never be seen. The refusal must fail the
// check and name the model it refused.
func TestAVertexCheckCountsEveryModelItsRolesCall(t *testing.T) {
	const path = "/v1/projects/example-project/locations/us/publishers/google/models/"
	fake := &countTokensFake{answers: map[string]int{"gemini-concierge-example": http.StatusForbidden}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s := vertexChecked(srv, "gemini-summary-example", "", "gemini-concierge-example")
	asked, _ := fake.requests()
	want := []string{
		"POST " + path + "gemini-summary-example:countTokens Bearer gt-1",
		"POST " + path + "gemini-concierge-example:countTokens Bearer gt-1",
	}
	if strings.Join(asked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the check asked %q; want a count of each model the roles call, in their order: %q", asked, want)
	}
	if s.OK || !s.Asked || !strings.Contains(s.Message, "gemini-concierge-example") || !strings.Contains(s.Message, "HTTP 403") {
		t.Fatalf("a provider that counted the summary model and refused the Concierge's: %+v; want not OK, asked, naming the refused model and its HTTP 403", s)
	}
}

// TestAGoogleTextToSpeechCheckListsVoicesWithTheToken fails if a Google Cloud
// Text-to-Speech provider signed in by Google stops at minting a token
// instead of listing voices with it, or reads a refusal as working or quotes
// it.
func TestAGoogleTextToSpeechCheckListsVoicesWithTheToken(t *testing.T) {
	var refuse atomic.Bool
	var seen authRecorder
	var got string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		mu.Lock()
		got = r.Method + " " + r.URL.RequestURI()
		mu.Unlock()
		if refuse.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"caller %s turned away"}}`, r.Header.Get("Authorization"))
			return
		}
		_, _ = io.WriteString(w, `{"voices":[]}`)
	}))
	defer srv.Close()
	p, _ := New(&config.Config{Providers: map[string]config.Provider{
		"speech": {API: config.APIGoogleTTS, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
	}}, Options{GoogleCredentials: staticGoogle}).Get("speech")
	s := p.Check(context.Background())
	mu.Lock()
	request := got
	mu.Unlock()
	if !s.OK || !s.Asked || request != "GET /v1/voices?languageCode=en-US" || strings.Join(seen.all(), ",") != "Authorization: Bearer gt-1" {
		t.Fatalf("check %+v after %q sent %q; want OK and asked, listing voices with the token", s, request, seen.all())
	}
	refuse.Store(true)
	s = p.Check(context.Background())
	if s.OK || !s.Asked || !strings.Contains(s.Message, "refused the credential (HTTP 403)") || strings.Contains(s.Message, "gt-1") || strings.Contains(s.Message, "turned away") {
		t.Fatalf("a refusal: %+v; want not OK, asked, saying the 403 refused the credential, and quoting nothing", s)
	}
}
