package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/stopping"
)

// getJSON sends GET path to h and decodes its JSON answer into v.
func getJSON(t *testing.T, h http.Handler, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("decode GET %s: %v: %s", path, err, rr.Body.String())
	}
	return rr
}

// TestGetConfigServesEachProviderAndNoSecret fails if GET /api/config does not
// serve each provider as the config names it (its API, url, project,
// location and sign-in), and the text-to-speech role; if it serves a
// provider's key; or if it keeps a field the providers replaced: api_keys,
// summarization.base_url.
func TestGetConfigServesEachProviderAndNoSecret(t *testing.T) {
	path := writeProvidersConfig(t)
	t.Setenv(config.EnvPrefix+"DEEPGRAM_API_KEY", "dg-1")
	cfgStore, _, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// The control: the unit holds the key, so an answer that served it
	// would hold it.
	if cfg := cfgStore.Get(); cfg.APIKey("deepgram") != "dg-1" {
		t.Fatalf("deepgram's key = %q, want dg-1: the check below could not see a served key", cfg.APIKey("deepgram"))
	}
	h := newConfigHandler(t, cfgStore, &ControlHooks{})

	var resp map[string]json.RawMessage
	rr := getJSON(t, h, "/api/config", &resp)
	if strings.Contains(rr.Body.String(), "dg-1") {
		t.Errorf("GET /api/config serves deepgram's key: %s", rr.Body.String())
	}
	if _, ok := resp["api_keys"]; ok {
		t.Errorf("GET /api/config still serves api_keys: %s", resp["api_keys"])
	}

	var served map[string]config.Provider
	if err := json.Unmarshal(resp["providers"], &served); err != nil {
		t.Fatalf("providers %s: %v", resp["providers"], err)
	}
	want := cfgStore.Get().Providers
	if len(want) != 5 || want["gateway"].URL != "https://gateway.example.test/v1" || want["openai"].Auth.IdentityProviderID != "idp-example" {
		t.Fatalf("the config holds providers %+v, not providersYAML's", want)
	}
	if !reflect.DeepEqual(served, want) {
		t.Errorf("providers = %+v,\nwant %+v", served, want)
	}

	var tts map[string]any
	if err := json.Unmarshal(resp["tts"], &tts); err != nil {
		t.Fatalf("tts %s: %v", resp["tts"], err)
	}
	if wantTTS := map[string]any{"model": "vertex-us/gemini-3.8-flash-tts", "voice": "Kore"}; !reflect.DeepEqual(tts, wantTTS) {
		t.Errorf("tts = %v, want %v", tts, wantTTS)
	}

	var summarization map[string]json.RawMessage
	if err := json.Unmarshal(resp["summarization"], &summarization); err != nil {
		t.Fatalf("summarization %s: %v", resp["summarization"], err)
	}
	if got := slices.Sorted(maps.Keys(summarization)); !slices.Equal(got, []string{"model", "presets"}) {
		t.Errorf("summarization fields %v, want exactly model and presets", got)
	}
}

// TestPatchConfigSavesAProviderKey fails if PATCH /api/config's provider_keys
// does not give an api_key provider its key, does not write it to the .env
// file under the variable the provider's auth.env names, or answers with it.
func TestPatchConfigSavesAProviderKey(t *testing.T) {
	path := writeProvidersConfig(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	cfgStore, _, err := config.NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	h := newConfigHandler(t, cfgStore, &ControlHooks{})

	rr := patchConfig(h, `{"provider_keys":{"deepgram":"dg-2"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH provider_keys = %d %s, want 200", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "dg-2") {
		t.Errorf("PATCH /api/config answers with the key it saved: %s", rr.Body.String())
	}
	if cfg := cfgStore.Get(); cfg.APIKey("deepgram") != "dg-2" {
		t.Errorf("deepgram's key = %q, want dg-2", cfg.APIKey("deepgram"))
	}
	env, err := os.ReadFile(envPath)
	if err != nil || !strings.Contains(string(env), config.EnvPrefix+"DEEPGRAM_API_KEY=dg-2\n") {
		t.Errorf(".env (%v) = %q, want it to hold %sDEEPGRAM_API_KEY=dg-2", err, env, config.EnvPrefix)
	}
}

// TestPatchConfigRefusesAKeyForAProviderThatSignsInAnotherWay fails if PATCH
// /api/config's provider_keys accepts a key for a provider that signs in by
// Google, a Google ID token or OpenAI workload identity, or for no provider;
// answers in another shape than {"error": ...} naming the provider; or
// changes anything when it refuses, a key it was given beside the refused
// one included.
func TestPatchConfigRefusesAKeyForAProviderThatSignsInAnotherWay(t *testing.T) {
	for _, tc := range []struct {
		body, provider string
	}{
		{`{"provider_keys":{"vertex":"k-1"}}`, "vertex"},
		{`{"provider_keys":{"gateway":"k-1"}}`, "gateway"},
		{`{"provider_keys":{"openai":"sk-1"}}`, "openai"},
		{`{"provider_keys":{"nowhere":"k-1"}}`, "nowhere"},
		{`{"provider_keys":{"deepgram":"dg-2","vertex":"k-1"}}`, "vertex"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			cfgStore, path := newProvidersConfigStore(t)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var announced atomic.Int32
			cfgStore.OnChange(func(config.Config) { announced.Add(1) })
			h := newConfigHandler(t, cfgStore, &ControlHooks{})

			rr := patchConfig(h, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("PATCH %s = %d %s, want 400", tc.body, rr.Code, rr.Body.String())
			}
			var refusal map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &refusal); err != nil || len(refusal) != 1 || !strings.Contains(refusal["error"], tc.provider) {
				t.Errorf("refusal %s (%v), want {\"error\": ...} naming %s", rr.Body.String(), err, tc.provider)
			}
			if keys := cfgStore.Get().ProviderKeys; len(keys) != 0 {
				t.Errorf("a refused PATCH set keys %v", slices.Sorted(maps.Keys(keys)))
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
				t.Errorf("a refused PATCH rewrote the config file (%v)", err)
			}
			if n := announced.Load(); n != 0 {
				t.Errorf("a refused PATCH announced %d config changes", n)
			}
		})
	}
}

// TestPatchConfigSavesTheTextToSpeechRole fails if PATCH /api/config's tts
// does not set tts.model and tts.voice in the config, its file and the
// answer, changes the one a patch does not name, or accepts a model no
// provider serves.
func TestPatchConfigSavesTheTextToSpeechRole(t *testing.T) {
	cfgStore, path := newProvidersConfigStore(t)
	h := newConfigHandler(t, cfgStore, &ControlHooks{})

	for _, step := range []struct {
		body         string
		model, voice string
	}{
		{`{"tts":{"model":"gateway/tts-example","voice":"alloy"}}`, "gateway/tts-example", "alloy"},
		{`{"tts":{"voice":"Puck"}}`, "gateway/tts-example", "Puck"},
	} {
		rr := patchConfig(h, step.body)
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH %s = %d %s, want 200", step.body, rr.Code, rr.Body.String())
		}
		var resp struct {
			TTS    map[string]string `json:"tts"`
			Models map[string]string `json:"models"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if want := map[string]string{"model": step.model, "voice": step.voice}; !reflect.DeepEqual(resp.TTS, want) {
			t.Errorf("after PATCH %s: tts = %v, want %v", step.body, resp.TTS, want)
		}
		if resp.Models["tts"] != step.model || resp.Models["tts_voice"] != step.voice {
			t.Errorf("after PATCH %s: models.tts %q, models.tts_voice %q; want %q, %q", step.body, resp.Models["tts"], resp.Models["tts_voice"], step.model, step.voice)
		}
	}
	reloaded, _, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get().TTS; got.Model != "gateway/tts-example" || got.Voice != "Puck" {
		t.Errorf("tts saved in the config file = %+v, want gateway/tts-example, Puck", got)
	}

	for _, body := range []string{`{"tts":{"model":"nowhere/tts-example"}}`, `{"tts":{"model":"tts-example"}}`} {
		if rr := patchConfig(h, body); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "tts.model") {
			t.Errorf("PATCH %s = %d %s, want 400 naming tts.model", body, rr.Code, rr.Body.String())
		}
	}
	if got := cfgStore.Get().TTS.Model; got != "gateway/tts-example" {
		t.Errorf("a refused tts.model changed it to %q", got)
	}
}

// TestPatchConfigANewTTSModelSpeaksInItsOwnDefaultVoice fails if a PATCH
// /api/config that changes tts.model and names no tts.voice keeps the voice
// of the model before it (Kore stays on Aura, which then asks Deepgram for a
// model called Kore and every speak fails); if it drops a voice the patch
// names with the model, or the voice when the model does not change; or if a
// refused model change clears the voice.
func TestPatchConfigANewTTSModelSpeaksInItsOwnDefaultVoice(t *testing.T) {
	cfgStore, path := newProvidersConfigStore(t)
	h := newConfigHandler(t, cfgStore, &ControlHooks{})

	for _, step := range []struct {
		body         string
		model, voice string
	}{
		{`{"tts":{"model":"gateway/tts-example","voice":"Kore"}}`, "gateway/tts-example", "Kore"},
		{`{"tts":{"model":"gateway/tts-other"}}`, "gateway/tts-other", ""},
		{`{"tts":{"voice":"Puck"}}`, "gateway/tts-other", "Puck"},
		{`{"tts":{"model":"gateway/tts-other"}}`, "gateway/tts-other", "Puck"},
		{`{"tts":{"model":"gateway/tts-third","voice":"alloy"}}`, "gateway/tts-third", "alloy"},
	} {
		rr := patchConfig(h, step.body)
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH %s = %d %s, want 200", step.body, rr.Code, rr.Body.String())
		}
		var resp struct {
			TTS map[string]string `json:"tts"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if want := map[string]string{"model": step.model, "voice": step.voice}; !reflect.DeepEqual(resp.TTS, want) {
			t.Errorf("after PATCH %s: tts = %v, want %v", step.body, resp.TTS, want)
		}
	}
	if rr := patchConfig(h, `{"tts":{"model":"nowhere/tts-example"}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("PATCH of a model no provider serves = %d %s, want 400", rr.Code, rr.Body.String())
	}
	if got := cfgStore.Get().TTS; got.Model != "gateway/tts-third" || got.Voice != "alloy" {
		t.Errorf("after a refused tts.model: tts = %+v, want gateway/tts-third in alloy", got)
	}
	reloaded, _, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get().TTS; got.Model != "gateway/tts-third" || got.Voice != "alloy" {
		t.Errorf("tts saved in the config file = %+v, want gateway/tts-third in alloy", got)
	}
}

// TestPatchConfigRefusesTheRemovedFields fails if PATCH /api/config accepts a
// field the providers replaced (api_keys, summarization.base_url,
// voice.tts_engine) as if it had saved it, or changes anything when it
// refuses one.
func TestPatchConfigRefusesTheRemovedFields(t *testing.T) {
	for _, tc := range []struct {
		body, field string
	}{
		{`{"api_keys":{"deepgram":"dg-3"}}`, "api_keys"},
		{`{"summarization":{"base_url":"https://llm.example.test/v1"}}`, "base_url"},
		{`{"voice":{"tts_engine":"aura"}}`, "tts_engine"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			cfgStore, path := newProvidersConfigStore(t)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var announced atomic.Int32
			cfgStore.OnChange(func(config.Config) { announced.Add(1) })
			h := newConfigHandler(t, cfgStore, &ControlHooks{})

			rr := patchConfig(h, tc.body)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), tc.field) {
				t.Errorf("PATCH %s = %d %s, want 400 naming %s", tc.body, rr.Code, rr.Body.String(), tc.field)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
				t.Errorf("a refused PATCH rewrote the config file (%v)", err)
			}
			if n := announced.Load(); n != 0 {
				t.Errorf("a refused PATCH announced %d config changes", n)
			}
		})
	}
}

// testClock is a clock a test moves.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// providerJSON is one provider of GET /api/providers as a client reads it:
// its own type, so a field the handler names wrongly does not decode.
type providerJSON struct {
	providers.Status
	Roles []string `json:"roles"`
}

// TestProvidersRouteReportsEachProviderWithItsRolesAndCachesItsChecks fails if
// GET /api/providers does not report each provider's check with the roles
// that call it (an empty list for one no role calls), serves a key, checks a
// provider checked less than 5 minutes ago again, keeps a check 5 minutes
// old, or keeps any with refresh=1.
func TestProvidersRouteReportsEachProviderWithItsRolesAndCachesItsChecks(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	var deepgramAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		if r.URL.Path == "/v1/projects" {
			deepgramAuth = r.Header.Get("Authorization")
		}
		mu.Unlock()
		// The openai provider's check lists the models and calls the one
		// summarization.model names; the others read their own path.
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-example","object":"model"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"gpt-example","choices":[{"index":0,"message":{"role":"assistant","content":"r"},"finish_reason":"length"}]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	reads := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(hits)
	}

	newTestConfigStore(t) // clears the environment
	t.Setenv(config.EnvPrefix+"DEEPGRAM_API_KEY", "dg-1")
	t.Setenv(config.EnvPrefix+"OPENAI_API_KEY", "sk-1")
	t.Setenv(config.EnvPrefix+"SPARE_API_KEY", "el-1")
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, fmt.Appendf(nil, `providers:
  deepgram: {api: deepgram, url: %[1]s, auth: {kind: api_key, env: GHOST_WISPR_DEEPGRAM_API_KEY}}
  openai: {api: openai, url: %[1]s/v1, auth: {kind: api_key, env: GHOST_WISPR_OPENAI_API_KEY}}
  spare: {api: elevenlabs, url: %[1]s, auth: {kind: api_key, env: GHOST_WISPR_SPARE_API_KEY}}
transcription: {model: deepgram/nova-3}
summarization: {model: openai/gpt-example}
`, srv.URL), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgStore, _, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgStore.Get()
	clk := &testClock{now: time.Unix(1_800_000_000, 0)}
	set := providers.New(&cfg, providers.Options{Client: srv.Client(), Now: clk.Now})
	h := newConfigHandler(t, cfgStore, &ControlHooks{ProviderStatuses: set.Statuses})

	get := func(path string) []providerJSON {
		t.Helper()
		var resp struct {
			Providers []providerJSON `json:"providers"`
		}
		rr := getJSON(t, h, path, &resp)
		for _, key := range []string{"dg-1", "sk-1", "el-1"} {
			if strings.Contains(rr.Body.String(), key) {
				t.Errorf("GET %s serves the key %s: %s", path, key, rr.Body.String())
			}
		}
		return resp.Providers
	}
	checkedEach := func(when string, n int) {
		t.Helper()
		want := map[string]int{"/v1/projects": n, "/v1/models": n, "/v1/chat/completions": n, "/v1/user": n}
		if got := reads(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the providers were read %v, want %v", when, got, want)
		}
	}

	statuses := get("/api/providers")
	checkedEach("the first GET", 1)
	// The control for the key checks above: the check sent deepgram's key.
	if deepgramAuth != "Token dg-1" {
		t.Fatalf("deepgram's check sent Authorization %q, want its key", deepgramAuth)
	}
	wantRoles := map[string][]string{
		"deepgram": {"transcription.model", "batch_transcription.model"},
		"openai":   {"summarization.model"},
		"spare":    {},
	}
	var names []string
	for _, s := range statuses {
		names = append(names, s.Name)
		if !s.Checked || !s.OK || !s.KeySet {
			t.Errorf("%s: %+v, want checked, working and its key set", s.Name, s.Status)
		}
		if s.Roles == nil || !slices.Equal(s.Roles, wantRoles[s.Name]) {
			t.Errorf("%s roles = %#v, want %#v", s.Name, s.Roles, wantRoles[s.Name])
		}
	}
	if !slices.Equal(names, []string{"deepgram", "openai", "spare"}) {
		t.Errorf("providers %v, want deepgram, openai, spare", names)
	}

	clk.advance(5*time.Minute - time.Second)
	get("/api/providers")
	checkedEach("a GET 4 min 59 s later", 1)

	get("/api/providers?refresh=1")
	checkedEach("refresh=1", 2)

	clk.advance(5 * time.Minute)
	get("/api/providers")
	checkedEach("a GET 5 min after the last check", 3)
}

// TestProvidersRouteAnswers501WithoutItsHook fails if GET /api/providers
// answers anything but 501 {"error": ...} when nothing checks providers.
func TestProvidersRouteAnswers501WithoutItsHook(t *testing.T) {
	cfgStore, _ := newProvidersConfigStore(t)
	h := newConfigHandler(t, cfgStore, &ControlHooks{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	var refusal map[string]string
	if rr.Code != http.StatusNotImplemented || json.Unmarshal(rr.Body.Bytes(), &refusal) != nil || refusal["error"] == "" {
		t.Errorf("GET /api/providers = %d %s, want 501 {\"error\": ...}", rr.Code, rr.Body.String())
	}
}

// TestProvidersRouteChecksOutliveTheRequest fails if a page that closes while
// GET /api/providers checks cancels the checks: the cancelled check would be
// kept for 5 minutes as a failure.
func TestProvidersRouteChecksOutliveTheRequest(t *testing.T) {
	cfgStore, _ := newProvidersConfigStore(t)
	called := false
	var gotErr error
	h := newConfigHandler(t, cfgStore, &ControlHooks{ProviderStatuses: func(ctx context.Context, _ time.Duration) []providers.Status {
		called, gotErr = true, ctx.Err()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/providers", nil).WithContext(ctx))
	if !called {
		t.Fatalf("GET /api/providers = %d %s and checked nothing", rr.Code, rr.Body.String())
	}
	if gotErr != nil {
		t.Errorf("the checks ran with a context ended by the request (%v), want one that outlives it", gotErr)
	}
}

// TestProvidersRouteChecksEndWithTheStop fails if GET /api/providers' checks
// run on once the app's stop has cut off the request (its context ends with
// stopping.ErrStopped, as ControlHooks.AdmitRequest ends it): the stop waits
// for the request, for as long as a check takes. A page that closes still
// leaves them running (TestProvidersRouteChecksOutliveTheRequest, the
// control).
func TestProvidersRouteChecksEndWithTheStop(t *testing.T) {
	cfgStore, _ := newProvidersConfigStore(t)
	entered := make(chan struct{})
	h := newConfigHandler(t, cfgStore, &ControlHooks{ProviderStatuses: func(ctx context.Context, _ time.Duration) []providers.Status {
		close(entered)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return nil
	}})
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/providers", nil).WithContext(ctx))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("control: GET /api/providers checked nothing")
	}
	stop(stopping.ErrStopped)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("GET /api/providers ran on after the app's stop cut its request off: the stop waits for its checks")
	}
}

// TestOpenAPISpecDocumentsTheProviders fails if the OpenAPI spec does not
// document /api/config's read-only providers, write-only provider_keys, tts
// role, summarization without base_url and each models key, GET
// /api/providers with refresh and roles, and POST /api/tts/speak without
// provider.
func TestOpenAPISpecDocumentsTheProviders(t *testing.T) {
	raw, err := OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	properties := func(v any) []string {
		m, _ := v.(map[string]any)
		return slices.Sorted(maps.Keys(m))
	}
	for _, schema := range [][]string{
		{"paths", "/api/config", "get", "responses", "200", "content", "application/json", "schema", "properties"},
		{"paths", "/api/config", "patch", "requestBody", "content", "application/json", "schema", "properties"},
	} {
		at := func(keys ...string) any { return dig(t, spec, append(slices.Clone(schema), keys...)...) }
		if at("providers", "readOnly") != true {
			t.Errorf("%v providers is not readOnly", schema[:3])
		}
		if got := properties(at("providers", "additionalProperties", "properties")); !slices.Equal(got, []string{"api", "auth", "location", "project", "url"}) {
			t.Errorf("%v a provider's properties %v, want api, auth, location, project, url", schema[:3], got)
		}
		if got := properties(at("providers", "additionalProperties", "properties", "auth", "properties")); !slices.Equal(got, []string{"audience", "env", "identity_provider_id", "kind", "service_account_id"}) {
			t.Errorf("%v a provider's auth properties %v", schema[:3], got)
		}
		if at("provider_keys", "writeOnly") != true {
			t.Errorf("%v provider_keys is not writeOnly", schema[:3])
		}
		if got := properties(at("tts", "properties")); !slices.Equal(got, []string{"model", "voice"}) {
			t.Errorf("%v tts properties %v, want model and voice", schema[:3], got)
		}
		if got := properties(at("summarization", "properties")); !slices.Equal(got, []string{"model", "presets"}) {
			t.Errorf("%v summarization properties %v, want model and presets", schema[:3], got)
		}
		description, _ := at("models", "description").(string)
		for _, key := range []string{"live_transcription", "batch_transcription", "summarization", "embedding", "concierge", "tts", "tts_voice", "gptlive", "dgagent_provider"} {
			if !strings.Contains(description, key) {
				t.Errorf("%v models description does not name %s: %q", schema[:3], key, description)
			}
		}
		for _, gone := range []string{"genai_backend", "gcp_project", "tts_provider", "batch_transcription_provider"} {
			if strings.Contains(description, gone) {
				t.Errorf("%v models description still names %s", schema[:3], gone)
			}
		}
	}

	get, _ := dig(t, spec, "paths", "/api/providers", "get").(map[string]any)
	if get == nil {
		t.Fatal("GET /api/providers is not in the OpenAPI spec")
	}
	params, _ := get["parameters"].([]any)
	var refresh bool
	for _, p := range params {
		if m, _ := p.(map[string]any); m["name"] == "refresh" && m["in"] == "query" {
			refresh = true
		}
	}
	if !refresh {
		t.Errorf("GET /api/providers parameters %v name no refresh query", params)
	}
	item := dig(t, get, "responses", "200", "content", "application/json", "schema", "properties", "providers", "items", "properties")
	for _, field := range []string{"name", "api", "url", "auth", "sign_in", "key_env", "key_set", "checked", "asked", "ok", "message", "checked_at", "roles"} {
		if m, _ := item.(map[string]any); m[field] == nil {
			t.Errorf("GET /api/providers' provider has no %s", field)
		}
	}
	if dig(t, get, "responses", "501") == nil {
		t.Errorf("GET /api/providers documents no 501")
	}

	speak := dig(t, spec, "paths", "/api/tts/speak", "post", "requestBody", "content", "application/json", "schema", "properties")
	if got := properties(speak); !slices.Equal(got, []string{"text", "voice"}) {
		t.Errorf("POST /api/tts/speak body properties %v, want text and voice", got)
	}
}
