package main

// The unit's providers as main wires them: a key saved on the settings page
// reaching the providers check, the speak API following the tts role saved
// last, voices B and C dialling the provider their settings name, live
// transcription dialling its provider's url, and when summaries can run.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/tts"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestASavedKeyReachesTheProvidersCheck fails if a key saved with PATCH
// /api/config's provider_keys does not reach GET /api/providers?refresh=1 as
// main wires them (the config store loadRuntimeConfig builds, with its .env,
// and providerStatuses of that store): the settings page would keep showing
// a refused key after the operator saved a working one.
func TestASavedKeyReachesTheProvidersCheck(t *testing.T) {
	const keyEnv = "EXAMPLE_DEEPGRAM_KEY"
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		asked = append(asked, r.URL.Path+" "+auth)
		mu.Unlock()
		if r.URL.Path != "/v1/projects" || auth != "Token dg-2" {
			http.Error(w, `{"err_code":"INVALID_AUTH"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"projects":[]}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(keyEnv, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "ghost-wispr.yaml")
	if err := os.WriteFile(path, fmt.Appendf(nil, "providers:\n  deepgram: {api: deepgram, url: %s, auth: {kind: api_key, env: %s}}\ntranscription: {model: deepgram/nova-3}\n", srv.URL, keyEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(keyEnv+"=dg-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgStore, _, err := loadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), nil,
		&server.ControlHooks{ProviderStatuses: providerStatuses(cfgStore)}, "", nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}
	deepgram := func(path string) providers.Status {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		var resp struct {
			Providers []providers.Status `json:"providers"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &resp) != nil {
			t.Fatalf("GET %s = %d %s", path, rr.Code, rr.Body.String())
		}
		for _, s := range resp.Providers {
			if s.Name == "deepgram" {
				return s
			}
		}
		t.Fatalf("GET %s names no deepgram provider: %s", path, rr.Body.String())
		return providers.Status{}
	}

	// The control: the fake refuses the first key, so the check can fail.
	if before := deepgram("/api/providers"); !before.Checked || before.OK {
		t.Fatalf("with the refused key dg-1: %+v, want checked and failing", before)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"provider_keys":{"deepgram":"dg-2"}}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH provider_keys = %d %s", rr.Code, rr.Body.String())
	}
	if after := deepgram("/api/providers?refresh=1"); !after.OK || !after.KeySet {
		t.Fatalf("after the key was saved, GET /api/providers?refresh=1: %+v, want it working", after)
	}
	mu.Lock()
	defer mu.Unlock()
	if last := asked[len(asked)-1]; last != "/v1/projects Token dg-2" {
		t.Fatalf("the fake Deepgram was last asked %q (all: %q), want the projects read with the saved key", last, asked)
	}
	env, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil || !strings.Contains(string(env), keyEnv+"=dg-2") {
		t.Fatalf(".env after the save = %q, %v; want the saved key under %s", env, err, keyEnv)
	}
}

// heldConfig is a config the test changes in place, as the settings page
// and a start with an invalid tts.model leave it: speechSource reads it.
type heldConfig struct {
	mu  sync.Mutex
	cfg config.Config
}

func (h *heldConfig) Get() config.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.cfg
	c.Providers, c.ProviderKeys = maps.Clone(c.Providers), maps.Clone(c.ProviderKeys)
	return c
}

func (h *heldConfig) change(f func(*config.Config)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.cfg)
}

// speechCall is one request a fake OpenAI speech API was sent.
type speechCall struct {
	path, auth, model, voice, input string
}

// speechAPI is a fake OpenAI /audio/speech that answers PCM.
type speechAPI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []speechCall
}

func startSpeechAPI(t *testing.T) *speechAPI {
	t.Helper()
	f := &speechAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model, Voice, Input string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, speechCall{r.URL.Path, r.Header.Get("Authorization"), body.Model, body.Voice, body.Input})
		f.mu.Unlock()
		_, _ = w.Write(make([]byte, 480))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// take returns the calls since the last take.
func (f *speechAPI) take() []speechCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

// TestSpeechFollowsTheTTSRoleSavedLast fails if POST /api/tts/speak's
// provider (speechSource) keeps the engine, model, voice or key it first
// built after a settings change of tts.model (off to on, a new model, an
// invalid one then a valid one), tts.voice or the provider's key; or speaks
// while tts.model is off.
func TestSpeechFollowsTheTTSRoleSavedLast(t *testing.T) {
	const keyEnv = "EXAMPLE_SPEECH_KEY"
	api := startSpeechAPI(t)
	held := &heldConfig{cfg: config.Config{
		Providers: map[string]config.Provider{
			"speech": {API: config.APIOpenAI, URL: api.srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
		},
		ProviderKeys: map[string]string{keyEnv: "sk-1"},
	}}
	source := &speechSource{store: held}
	speak := func(step string, want *speechCall) {
		t.Helper()
		resp, err := source.Synthesize(t.Context(), tts.SpeechRequest{Text: "hello " + step})
		calls := api.take()
		if want == nil {
			if err == nil || len(calls) != 0 {
				t.Fatalf("%s: Synthesize = %v, %v with %d requests; want an error and no request", step, resp, err, len(calls))
			}
			return
		}
		if err != nil || len(calls) != 1 {
			t.Fatalf("%s: Synthesize = %v with requests %+v; want one request", step, err, calls)
		}
		want.path, want.input = "/v1/audio/speech", "hello "+step
		if calls[0] != *want {
			t.Fatalf("%s: the provider was sent %+v, want %+v", step, calls[0], *want)
		}
		if resp.Model != want.model {
			t.Errorf("%s: the answer names model %q, want %q", step, resp.Model, want.model)
		}
	}

	speak("tts.model off", nil)
	if source.Configured() {
		t.Error("with tts.model off, the speak API's provider says it is configured")
	}
	held.change(func(c *config.Config) { c.TTS.Model = "speech/tts-a" })
	if !source.Configured() {
		t.Error("with tts.model on, the speak API's provider says it is not configured")
	}
	speak("tts.model on", &speechCall{auth: "Bearer sk-1", model: "tts-a", voice: "marin"})
	held.change(func(c *config.Config) { c.TTS.Model = "speech/tts-b" })
	speak("a new model", &speechCall{auth: "Bearer sk-1", model: "tts-b", voice: "marin"})
	held.change(func(c *config.Config) { c.TTS.Voice = "coral" })
	speak("a new voice", &speechCall{auth: "Bearer sk-1", model: "tts-b", voice: "coral"})
	held.change(func(c *config.Config) { c.ProviderKeys[keyEnv] = "sk-2" })
	speak("a new key", &speechCall{auth: "Bearer sk-2", model: "tts-b", voice: "coral"})
	held.change(func(c *config.Config) { c.TTS.Model = "missing/tts-c" })
	speak("a model no provider serves", nil)
	held.change(func(c *config.Config) { c.TTS.Model = "speech/tts-d" })
	speak("a valid model after an invalid one", &speechCall{auth: "Bearer sk-2", model: "tts-d", voice: "coral"})
	held.change(func(c *config.Config) { c.TTS.Model = "" })
	if source.Configured() {
		t.Error("with tts.model turned off again, the speak API's provider says it is configured")
	}
	speak("tts.model turned off again", nil)
}

// The orchestrator asks speechSource whether text-to-speech is on: with it
// off, POST /api/tts/speak answers 501 and queues nothing.
var _ tts.Configurable = (*speechSource)(nil)

// socketCall is what a fake provider socket was asked: its path, the
// credential, and the first message the client sent.
type socketCall struct {
	path, auth string
	first      []byte
}

// startProviderSocket is a provider's WebSocket that records one session's
// opening: it sends greeting, reads the client's first message, and closes,
// so the client's Open fails after it has said what it opens.
func startProviderSocket(t *testing.T, greeting string) (calls chan socketCall, url string) {
	t.Helper()
	calls = make(chan socketCall, 4)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := socketCall{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			calls <- call
			return
		}
		defer func() { _ = c.Close() }()
		if greeting != "" {
			_ = c.WriteMessage(websocket.TextMessage, []byte(greeting))
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, call.first, _ = c.ReadMessage()
		calls <- call
	}))
	t.Cleanup(srv.Close)
	return calls, srv.URL
}

func takeSocketCall(t *testing.T, calls chan socketCall) socketCall {
	t.Helper()
	select {
	case c := <-calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the fake provider socket was not dialled")
		return socketCall{}
	}
}

// silentOutput is a speaker output that plays nothing.
type silentOutput struct{}

func (silentOutput) Begin() voice.Playback { return silentOutput{} }
func (silentOutput) Write(context.Context, []byte, audio.AudioFormat) error {
	return nil
}
func (silentOutput) Drain(context.Context) error { return nil }

// openExchange opens v for one exchange; the fake provider ends it.
func openExchange(t *testing.T, v voice.Voice) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s, err := v.Open(ctx, &voice.Exchange{SessionID: "s1", Out: silentOutput{}, Tools: unreachableTools{errors.New("no tools in this test")}})
	if err == nil {
		_ = s.Close()
	}
}

// TestVoiceBDialsTheProviderItsModelNames fails if voice B, as newVoiceB
// builds it, does not open GPT-Live's session at the url of the provider
// voice.gptlive.model names (here not one named openai), with that
// provider's credential and the model as the provider names it.
func TestVoiceBDialsTheProviderItsModelNames(t *testing.T) {
	const keyEnv = "EXAMPLE_LIVE_KEY"
	calls, url := startProviderSocket(t, "")
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"office-live": {API: config.APIOpenAI, URL: url + "/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
		},
		ProviderKeys: map[string]string{keyEnv: "sk-1"},
		Voice:        config.Voice{Engine: config.VoiceEngineGPTLive, GPTLive: config.GPTLive{Model: "office-live/gpt-live-example"}},
	}
	v, err := newVoiceB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	openExchange(t, v)
	call := takeSocketCall(t, calls)
	var start struct {
		Type    string `json:"type"`
		Session struct {
			Model string `json:"model"`
		} `json:"session"`
	}
	_ = json.Unmarshal(call.first, &start)
	if call.path != "/v1/live/sessions" || call.auth != "Bearer sk-1" || start.Session.Model != "gpt-live-example" {
		t.Fatalf("voice B dialled %q with Authorization %q and started %s; want /v1/live/sessions, the provider's key and model gpt-live-example", call.path, call.auth, call.first)
	}
}

// TestVoiceCDialsTheProviderItsSettingsName fails if voice C, as newVoiceC
// builds it, does not open Deepgram's Voice Agent at the url of the provider
// voice.dgagent.provider names (here not one named deepgram), with that
// provider's key and voice.dgagent.think_model in its Settings.
func TestVoiceCDialsTheProviderItsSettingsName(t *testing.T) {
	const keyEnv = "EXAMPLE_AGENT_KEY"
	calls, url := startProviderSocket(t, `{"type":"Welcome","request_id":"r1"}`)
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"office-agent": {API: config.APIDeepgram, URL: url, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
		},
		ProviderKeys: map[string]string{keyEnv: "dg-1"},
		Voice: config.Voice{Engine: config.VoiceEngineDGAgent, WakePhrases: []string{"Wispr"},
			DGAgent: config.DGAgent{Provider: "office-agent", ThinkModel: "google/gemini-example"}},
	}
	v, err := newVoiceC(cfg)
	if err != nil {
		t.Fatal(err)
	}
	openExchange(t, v)
	call := takeSocketCall(t, calls)
	var settings struct {
		Type  string `json:"type"`
		Agent struct {
			Think []struct {
				Provider struct{ Type, Model string } `json:"provider"`
			} `json:"think"`
		} `json:"agent"`
	}
	_ = json.Unmarshal(call.first, &settings)
	if call.path != "/v1/agent/converse" || call.auth != "token dg-1" || settings.Type != "Settings" ||
		len(settings.Agent.Think) == 0 || settings.Agent.Think[0].Provider.Type != "google" || settings.Agent.Think[0].Provider.Model != "gemini-example" {
		t.Fatalf("voice C dialled %q with Authorization %q and sent %s; want /v1/agent/converse, the provider's key and think model google/gemini-example", call.path, call.auth, call.first)
	}
}

// TestLiveTranscriptionHostIsTheProvidersSocket fails if the live
// transcription client's host is not the socket of transcription.model's
// provider url (ws for http, wss for https), or is not Deepgram's own ("")
// when the provider names no url.
func TestLiveTranscriptionHostIsTheProvidersSocket(t *testing.T) {
	for _, c := range []struct{ url, want string }{
		{"http://127.0.0.1:8080", "ws://127.0.0.1:8080"},
		{"https://deepgram.example.test", "wss://deepgram.example.test"},
		{"", ""},
	} {
		cfg := deepgramKeyConfig("dg-1")
		p := cfg.Providers["deepgram"]
		p.URL = c.url
		cfg.Providers["deepgram"] = p
		if got := liveTranscriptionClientOptions(&cfg).Host; got != c.want {
			t.Errorf("url %q: live transcription host %q, want %q", c.url, got, c.want)
		}
	}
}

// TestLiveTranscriptionDialsTheProviderURL fails if live transcription, with
// the options main builds from transcription.model, does not dial its
// provider's url with the provider's key and the model as Deepgram names it.
func TestLiveTranscriptionDialsTheProviderURL(t *testing.T) {
	type dial struct {
		path, auth, model string
	}
	dials := make(chan dial, 4)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials <- dial{r.URL.Path, r.Header.Get("Authorization"), r.URL.Query().Get("model")}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	cfg := deepgramKeyConfig("dg-1")
	p := cfg.Providers["deepgram"]
	p.URL = srv.URL
	cfg.Providers["deepgram"] = p
	cfg.Transcription.Model = "deepgram/nova-2-example"

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rc := newLiveTranscription(ctx, fixedKey(cfg.LiveTranscriptionKey()), liveTranscriptionClientOptions(&cfg),
		liveTranscriptionOptions(&cfg, 16000, nil), liveTranscriptionConfig(&cfg, 16000), transcriptCallback{}, newWarningsRegistry(), nil)
	t.Cleanup(func() { _ = rc.Close() })
	select {
	case d := <-dials:
		if d.path != "/v1/listen" || d.auth != "token dg-1" || d.model != "nova-2-example" {
			t.Fatalf("live transcription dialled %q with Authorization %q and model %q; want /v1/listen, the provider's key and nova-2-example", d.path, d.auth, d.model)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live transcription did not dial its provider's url")
	}
}

// TestCanSummarizeOnlyWithAProviderThatSignsIn fails if summaries turn on
// for a model whose provider lacks its key or cannot summarize, or stay off
// when summarization.model or only a preset's model names a provider that
// signs in.
func TestCanSummarizeOnlyWithAProviderThatSignsIn(t *testing.T) {
	const keyEnv = "EXAMPLE_SUMMARY_KEY"
	base := func(key string) config.Config {
		cfg := config.Config{Providers: map[string]config.Provider{
			"llm":    {API: config.APIOpenAI, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
			"vertex": {API: config.APIVertex, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}},
			"dg":     {API: config.APIDeepgram, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
		}}
		if key != "" {
			cfg.ProviderKeys = map[string]string{keyEnv: key}
		}
		return cfg
	}
	for _, c := range []struct {
		name    string
		key     string
		model   string
		presets map[string]config.Preset
		want    bool
	}{
		{name: "summarization.model with its key", key: "sk-1", model: "llm/gpt-example", want: true},
		{name: "summarization.model by workload identity", model: "vertex/gemini-example", want: true},
		{name: "only a preset's model", key: "sk-1", presets: map[string]config.Preset{"detailed": {Model: "llm/gpt-example"}}, want: true},
		{name: "summarization.model without its key", model: "llm/gpt-example", want: false},
		{name: "a preset's model without its key", model: "llm/gpt-example", presets: map[string]config.Preset{"detailed": {Model: "llm/gpt-other"}}, want: false},
		{name: "a provider that cannot summarize", key: "dg-1", model: "dg/nova-3", want: false},
		{name: "no model", key: "sk-1", presets: map[string]config.Preset{"detailed": {}}, want: false},
	} {
		cfg := base(c.key)
		cfg.Summarization = config.Summarization{Model: c.model, Presets: c.presets}
		if got := canSummarize(&cfg); got != c.want {
			t.Errorf("%s: canSummarize = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTheProvidersFollowTheModelsTheirRolesCall fails if a settings change of
// a role's model keeps the providers.Set built for the model before (its
// checks would read the old model: a Vertex AI check counts tokens of the
// models its roles call), if a change of nothing rebuilds it, or if a change
// of another role's model rebuilds the voice, which signs in again for each
// new build.
func TestTheProvidersFollowTheModelsTheirRolesCall(t *testing.T) {
	cfg := deepgramKeyConfig("dg-1")
	cfg.Providers["vertex"] = config.Provider{API: config.APIVertex, Project: "example-project", Location: "global", Auth: config.ProviderAuth{Kind: config.AuthGoogle}}
	cfg.Summarization.Model = "vertex/gemini-a"
	cfg.Voice.Engine = config.VoiceEngineDGAgent
	first := unitProviders.of(&cfg)
	var voices voiceSource
	voices.apply(t.Context(), &cfg)
	voice, err := voices.Voice()
	if err != nil {
		t.Fatal(err)
	}
	if again := unitProviders.of(&cfg); again != first {
		t.Error("the same settings built a second providers.Set")
	}

	cfg.Summarization.Model = "vertex/gemini-b"
	if changed := unitProviders.of(&cfg); changed == first {
		t.Error("a new summarization.model kept the providers.Set built for the model before")
	}
	voices.apply(t.Context(), &cfg)
	if kept, _ := voices.Voice(); kept != voice {
		t.Error("a new summarization.model rebuilt voice C")
	}
}
