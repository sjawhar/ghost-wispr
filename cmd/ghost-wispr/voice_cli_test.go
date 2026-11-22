package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/googleauthtest"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// firstAudioPCM is one event's or one Flush's audio: 100 ms of PCM16 mono at
// 24 kHz, more than one whole frame.
var firstAudioPCM = bytes.Repeat([]byte{1, 0}, 2400)

// fakeURLs are the fakes a unit's providers point at; "" is an address that
// answers nothing.
type fakeURLs struct{ vertex, deepgram string }

// firstAudioUnit writes a unit's ghost-wispr.yaml, its providers pointing at
// nothing, with extra appended; firstAudioUnitAt points them at fakes.
func firstAudioUnit(t *testing.T, extra string) string {
	t.Helper()
	return firstAudioUnitAt(t, fakeURLs{}, extra)
}

// firstAudioUnitAt writes a unit's ghost-wispr.yaml, whose text-to-speech is
// Gemini on Vertex AI's us multi-region with voice Kore, with Vertex AI in
// us and global and Deepgram at the fakes, and extra appended. It sets the
// environment the unit's .env would: a Deepgram key, Vertex AI credentials
// that need no network, and a Gemini API key for the Concierge's model
// client. It returns the config's path.
func firstAudioUnitAt(t *testing.T, urls fakeURLs, extra string) string {
	t.Helper()
	dir := t.TempDir()
	credentials := googleauthtest.X509CredentialFile(t, vertexTestToken)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	t.Setenv("GHOST_WISPR_GOOGLE_CREDENTIALS_FILE", credentials)
	t.Setenv("GHOST_WISPR_DEEPGRAM_API_KEY", strings.Repeat("k", 8))
	t.Setenv(exampleGeminiKeyEnv, strings.Repeat("g", 8))
	t.Setenv("GHOST_WISPR_CONCIERGE_MODEL", "gemini/gemini-test")
	t.Setenv("GHOST_WISPR_DISPATCH_TOKEN", "")
	t.Setenv("GHOST_WISPR_CONCIERGE_DISPATCH_URL", "")
	t.Setenv("GHOST_WISPR_CONCIERGE_ENVOY_URL", "")
	nowhere := "http://127.0.0.1:1"
	vertex, deepgram := cmp.Or(urls.vertex, nowhere), cmp.Or(urls.deepgram, nowhere)
	yaml := fmt.Sprintf(`db_path: %s
providers:
  vertex: {api: vertex, project: example-project, location: global, url: %q, auth: {kind: google}}
  vertex-us: {api: vertex, project: example-project, location: us, url: %q, auth: {kind: google}}
  deepgram: {api: deepgram, url: %q, auth: {kind: api_key, env: GHOST_WISPR_DEEPGRAM_API_KEY}}
  gemini: {api: gemini, auth: {kind: api_key, env: %s}}
tts:
  model: vertex-us/gemini-3.8-flash-tts
  voice: Kore
%s`, filepath.Join(dir, "ghost-wispr.db"), vertex, vertex, deepgram, exampleGeminiKeyEnv, extra)
	path := filepath.Join(dir, "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type cliRun struct {
	stdout, stderr string
	code           int
}

// cliOutput is a command's stdout or stderr. The Concierge logs from
// goroutines that can outlive issue-holder's return, as a process's would
// write until it exits, so a write and the test's read may overlap.
type cliOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *cliOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *cliOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func runFirstAudio(t *testing.T, configPath string, vendors *voiceAOptions, stdin string, args ...string) cliRun {
	t.Helper()
	var stdout, stderr cliOutput
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code := runTTSFirstAudioCommand(ctx, append([]string{"tts-first-audio"}, args...), configPath, vendors, strings.NewReader(stdin), &stdout, &stderr)
	return cliRun{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// printedLine is one line tts-first-audio printed, as accept-voice.sh
// reads it.
type printedLine struct {
	ms     float64
	optOut string // the raw JSON: true, false or null
}

// firstAudioLines reads every line of out, each one JSON object with a
// positive ms_to_first_frame and a mip_opt_out, and nothing else.
func firstAudioLines(t *testing.T, out string) []printedLine {
	t.Helper()
	var lines []printedLine
	for i, raw := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatalf("line %d %q is no JSON object: %v", i+1, raw, err)
		}
		var ms float64
		if err := json.Unmarshal(fields["ms_to_first_frame"], &ms); err != nil || ms <= 0 {
			t.Fatalf("line %d %q: ms_to_first_frame is not a positive number", i+1, raw)
		}
		optOut, ok := fields["mip_opt_out"]
		if !ok || len(fields) != 2 {
			t.Fatalf("line %d %q: want exactly ms_to_first_frame and mip_opt_out", i+1, raw)
		}
		lines = append(lines, printedLine{ms: ms, optOut: string(optOut)})
	}
	return lines
}

// fakeAura is Deepgram's /v1/speak: it keeps each socket's query and the
// texts spoken, and answers each Flush with firstAudioPCM and Flushed. With
// refuse it refuses every socket as a wrong key is.
type fakeAura struct {
	url    string
	mu     sync.Mutex
	opts   [][]string // each socket's mip_opt_out values
	spoken []string
}

func startFakeAura(t *testing.T, refuse bool) *fakeAura {
	t.Helper()
	f := &fakeAura{}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse {
			w.Header().Set("dg-error", "Invalid credentials.")
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.opts = append(f.opts, r.URL.Query()["mip_opt_out"])
		f.mu.Unlock()
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		for seq := 0; ; {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var m struct{ Type, Text string }
			_ = json.Unmarshal(data, &m)
			switch m.Type {
			case "Speak":
				f.mu.Lock()
				f.spoken = append(f.spoken, m.Text)
				f.mu.Unlock()
			case "Flush":
				_ = ws.WriteMessage(websocket.BinaryMessage, firstAudioPCM)
				_ = ws.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, `{"type":"Flushed","sequence_id":%d}`, seq))
				seq++
			case "Clear":
				_ = ws.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, `{"type":"Cleared","sequence_id":%d}`, seq))
			case "Close":
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL // the provider's url; Aura dials it as ws://
	return f
}

// TestTTSFirstAudioAuraReportsTheOptOutItsSocketCarried fails if
// tts-first-audio on an Aura model does not speak each non-empty line once
// and print one line for it, with a positive time to the first frame, or if
// the mip_opt_out it prints is not true while the sockets Deepgram saw
// carried mip_opt_out=true. The opt-out must come from the URL Aura dialled:
// with the parameter dropped from Aura's request, the line says false.
func TestTTSFirstAudioAuraReportsTheOptOutItsSocketCarried(t *testing.T) {
	deepgram := startFakeAura(t, false)
	run := runFirstAudio(t, firstAudioUnitAt(t, fakeURLs{deepgram: deepgram.url}, ""), nil,
		"One fact.\n\nTwo facts.\n   \nThree facts.\n", "--model", "deepgram/aura-2-thalia-en")
	if run.code != 0 {
		t.Fatalf("exit %d, stderr %q", run.code, run.stderr)
	}
	lines := firstAudioLines(t, run.stdout)
	deepgram.mu.Lock()
	defer deepgram.mu.Unlock()
	if want := []string{"One fact.", "Two facts.", "Three facts."}; !slices.Equal(deepgram.spoken, want) {
		t.Fatalf("Deepgram was asked to speak %q, want %q", deepgram.spoken, want)
	}
	if len(lines) != 3 {
		t.Fatalf("printed %d lines for 3 sentences: %q", len(lines), run.stdout)
	}
	if len(deepgram.opts) == 0 {
		t.Fatal("no socket reached Deepgram")
	}
	for i, opt := range deepgram.opts {
		if !slices.Equal(opt, []string{"true"}) {
			t.Errorf("socket %d reached Deepgram with mip_opt_out %q, want [\"true\"]", i+1, opt)
		}
	}
	for i, line := range lines {
		if line.optOut != "true" {
			t.Errorf("sentence %d: printed mip_opt_out %s; Deepgram saw the sockets' mip_opt_out as %q; want true from the dialled URL", i+1, line.optOut, deepgram.opts)
		}
	}
}

// fakeVertex is Vertex AI's streaming Gemini TTS. A stream sends one event of
// audio, then holds the rest for hold, or ends with none when silent. It
// keeps each request's path.
type fakeVertex struct {
	url    string
	hold   time.Duration
	status int  // a refusal of every speech request, when not 0
	silent bool // a stream that ends with no audio
	mu     sync.Mutex
	paths  []string
}

func startFakeVertex(t *testing.T, f *fakeVertex) *fakeVertex {
	t.Helper()
	event := func(data []byte) string {
		return fmt.Sprintf("data: {\"candidates\":[{\"content\":{\"parts\":[{\"inlineData\":{\"data\":%q}}]}}]}\n\n", base64.StdEncoding.EncodeToString(data))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			return // the warm-up
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+vertexTestToken {
			http.Error(w, `{"error":{"message":"no token"}}`, http.StatusUnauthorized)
			return
		}
		if f.status != 0 {
			http.Error(w, `{"error":{"message":"permission denied"}}`, f.status)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			w.Header().Set("Content-Type", "text/event-stream")
			if f.silent {
				return
			}
			_, _ = fmt.Fprint(w, event(firstAudioPCM))
			w.(http.Flusher).Flush()
			select {
			case <-time.After(f.hold):
			case <-r.Context().Done():
				return
			}
			_, _ = fmt.Fprint(w, event(firstAudioPCM))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeVertex) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths)
}

// TestTTSFirstAudioGeminiTimesTheFirstFrameAtTheModelsLocation fails if
// tts-first-audio on streaming Gemini does not stream from the location of
// the provider the model names (tts.model's when --model names none), times
// each sentence to the end of its stream instead of its first frame, or
// prints a mip_opt_out other than null.
func TestTTSFirstAudioGeminiTimesTheFirstFrameAtTheModelsLocation(t *testing.T) {
	const hold = 600 * time.Millisecond
	for _, tc := range []struct {
		name     string
		args     []string
		location string
	}{
		{"the unit's model", nil, "us"},
		{"--model on another location", []string{"--model", "vertex/gemini-3.8-flash-tts"}, "global"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vertex := startFakeVertex(t, &fakeVertex{hold: hold})
			run := runFirstAudio(t, firstAudioUnitAt(t, fakeURLs{vertex: vertex.url}, ""), nil, "One fact.\nTwo facts.\n", tc.args...)
			if run.code != 0 {
				t.Fatalf("exit %d, stderr %q", run.code, run.stderr)
			}
			lines := firstAudioLines(t, run.stdout)
			if len(lines) != 2 {
				t.Fatalf("printed %d lines for 2 sentences: %q", len(lines), run.stdout)
			}
			for i, line := range lines {
				if line.optOut != "null" {
					t.Errorf("sentence %d: mip_opt_out %s, want null for Gemini", i+1, line.optOut)
				}
				if line.ms >= float64(hold/time.Millisecond)/2 {
					t.Errorf("sentence %d: %.1f ms to the first frame; Vertex AI sent it at once and held the rest %v: it timed the stream, not the first frame", i+1, line.ms, hold)
				}
			}
			want := "/v1/projects/example-project/locations/" + tc.location + "/publishers/google/models/gemini-3.8-flash-tts:streamGenerateContent"
			if paths := vertex.requested(); len(paths) != 2 || paths[0] != want || paths[1] != want {
				t.Fatalf("Vertex AI was asked on %q, want %q twice", paths, want)
			}
		})
	}
}

// TestTTSFirstAudioClipTimesTheWholeClip fails if tts-first-audio --clip does
// not speak through POST /api/tts/speak's provider, which takes the whole
// stream before it plays, so it times a sentence before its whole clip came,
// or prints a mip_opt_out other than null for Gemini.
func TestTTSFirstAudioClipTimesTheWholeClip(t *testing.T) {
	const hold = 150 * time.Millisecond
	vertex := startFakeVertex(t, &fakeVertex{hold: hold})
	run := runFirstAudio(t, firstAudioUnitAt(t, fakeURLs{vertex: vertex.url}, ""), nil, "One fact.\nTwo facts.\n", "--clip")
	if run.code != 0 {
		t.Fatalf("exit %d, stderr %q", run.code, run.stderr)
	}
	lines := firstAudioLines(t, run.stdout)
	if len(lines) != 2 {
		t.Fatalf("printed %d lines for 2 sentences: %q", len(lines), run.stdout)
	}
	for i, line := range lines {
		if line.optOut != "null" {
			t.Errorf("sentence %d: mip_opt_out %s, want null for Gemini", i+1, line.optOut)
		}
		if line.ms < float64(hold/time.Millisecond) {
			t.Errorf("sentence %d: %.1f ms, less than the %v Vertex AI held the rest of the clip", i+1, line.ms, hold)
		}
	}
}

// TestTTSFirstAudioGivesAnotherModelItsOwnVoice fails if tts-first-audio on a
// --model other than tts.model speaks in tts.voice, which belongs to the
// unit's own model: the unit's Gemini voice Kore is no Aura model.
func TestTTSFirstAudioGivesAnotherModelItsOwnVoice(t *testing.T) {
	var mu sync.Mutex
	var models []string
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		models = append(models, r.URL.Query().Get("model"))
		mu.Unlock()
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil || bytes.Contains(data, []byte(`"Close"`)) {
				return
			}
			if bytes.Contains(data, []byte(`"Flush"`)) {
				_ = ws.WriteMessage(websocket.BinaryMessage, firstAudioPCM)
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"Flushed","sequence_id":0}`))
			}
		}
	}))
	t.Cleanup(srv.Close)
	run := runFirstAudio(t, firstAudioUnitAt(t, fakeURLs{deepgram: srv.URL}, ""), nil, "One fact.\n", "--model", "deepgram/aura-2-harmonia-en")
	if run.code != 0 {
		t.Fatalf("exit %d, stderr %q", run.code, run.stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(models) == 0 || !slices.Equal(slices.Compact(models), []string{"aura-2-harmonia-en"}) {
		t.Fatalf("Deepgram was asked for models %q, want the model --model names and not the unit's voice", models)
	}
}

// TestTTSFirstAudioEngineErrorExitsNonzero fails if an engine that refuses
// the unit leaves tts-first-audio exiting 0, or without the engine's error
// on stderr: Gemini names Vertex AI's status and not its message, which can
// quote the text spoken.
func TestTTSFirstAudioEngineErrorExitsNonzero(t *testing.T) {
	t.Run("aura", func(t *testing.T) {
		deepgram := startFakeAura(t, true)
		run := runFirstAudio(t, firstAudioUnitAt(t, fakeURLs{deepgram: deepgram.url}, ""), nil, "One fact.\n", "--model", "deepgram/aura-2-thalia-en")
		if run.code == 0 || !strings.Contains(run.stderr, "401") || run.stdout != "" {
			t.Fatalf("exit %d, stdout %q, stderr %q; want nonzero, nothing printed, and Deepgram's 401 on stderr", run.code, run.stdout, run.stderr)
		}
	})
	for name, args := range map[string][]string{"gemini": nil, "gemini --clip": {"--clip"}} {
		t.Run(name, func(t *testing.T) {
			vertex := startFakeVertex(t, &fakeVertex{status: http.StatusForbidden})
			run := runFirstAudio(t, firstAudioUnitAt(t, fakeURLs{vertex: vertex.url}, ""), nil, "One fact.\n", args...)
			if run.code == 0 || !strings.Contains(run.stderr, "HTTP 403") || strings.Contains(run.stderr, "permission denied") || run.stdout != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want nonzero, nothing printed, and HTTP 403 without Vertex AI's message on stderr", run.code, run.stdout, run.stderr)
			}
		})
	}
}

// TestTTSFirstAudioAnEngineWithNoAudioExitsNonzeroAtOnce fails if a sentence
// the engine answers with no audio leaves tts-first-audio exiting 0, or
// waiting seconds before it says so: voice A's Say returns once the text's
// first frame is queued, or once the text ended with none. The engine can say
// it returned no audio before the timer sees its empty sink, or the timer can
// say `the engine gave no audio`; both tell the caller the required fact and
// neither prints a result line.
func TestTTSFirstAudioAnEngineWithNoAudioExitsNonzeroAtOnce(t *testing.T) {
	vertex := startFakeVertex(t, &fakeVertex{silent: true})
	unit := firstAudioUnitAt(t, fakeURLs{vertex: vertex.url}, "")
	start := time.Now()
	run := runFirstAudio(t, unit, nil, "One fact.\n")
	took := time.Since(start)
	if run.code == 0 || !strings.Contains(run.stderr, "no audio") || run.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want nonzero, nothing printed, and no audio on stderr", run.code, run.stdout, run.stderr)
	}
	if took > 2*time.Second {
		t.Fatalf("took %v to report a sentence with no audio, want within 2 s", took)
	}
}

// TestTTSFirstAudioUsage fails if wrong arguments do not exit 2 with the
// usage on stderr, or answer with the light command's usage, which
// accept-voice.sh reads as a build without the subcommand.
func TestTTSFirstAudioUsage(t *testing.T) {
	configPath := firstAudioUnit(t, "")
	noModel := firstAudioUnit(t, "")
	data, err := os.ReadFile(noModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(noModel, []byte(strings.Replace(string(data), "model: vertex-us/gemini-3.8-flash-tts", `model: ""`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		config string
		args   []string
	}{
		"the old --engine":       {configPath, []string{"--engine", "aura"}},
		"an argument left over":  {configPath, []string{"extra"}},
		"--model without a name": {configPath, []string{"--model"}},
		"no model anywhere":      {noModel, nil},
	} {
		t.Run(name, func(t *testing.T) {
			run := runFirstAudio(t, c.config, nil, "One fact.\n", c.args...)
			if run.code != 2 || !strings.Contains(run.stderr, "usage: ghost-wispr tts-first-audio") || strings.Contains(run.stderr, "light-off") || run.stdout != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 2 and the usage", run.code, run.stdout, run.stderr)
			}
		})
	}
}

// startFakeDispatch is Dispatch with two issues: ABC-12, which agent session
// agent-live claims, and ABC-13, which nobody claims and whose newest event
// is the unit's own session's (own), after one of agent-live's. It refuses
// any token but token.
func startFakeDispatch(t *testing.T, token, own string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"invalid token","code":"unauthorized"}`)
			return
		}
		switch r.URL.Path {
		case "/api/v1/projects":
			_, _ = fmt.Fprint(w, `[{"key":"ABC"}]`)
		case "/api/v1/issues/ABC-12":
			_, _ = fmt.Fprint(w, `{"key":"ABC-12","title":"Ship it","claim":{"actor":{"kind":"session","id":"agent-live","origin":{"session_title":"Spec writer"}},"at":"2026-11-19T09:00:00Z"}}`)
		case "/api/v1/issues/ABC-12/events":
			_, _ = fmt.Fprint(w, `[]`)
		case "/api/v1/issues/ABC-13":
			_, _ = fmt.Fprint(w, `{"key":"ABC-13","title":"Nobody holds it"}`)
		case "/api/v1/issues/ABC-13/events":
			_, _ = fmt.Fprintf(w, `[{"type":"comment","created_at":"2026-11-19T10:00:00Z","actor":{"kind":"session","id":%q,"origin":{"session_title":"Ghost Wispr"}}},`+
				`{"type":"comment","created_at":"2026-11-19T09:00:00Z","actor":{"kind":"session","id":"agent-live","origin":{"session_title":"Spec writer"}}}]`, own)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":"not found","code":"not_found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeEnvoyRegistry is an Envoy listener whose registry has agent-live live.
// It keeps every other request: issue-holder must open no Envoy session,
// which would take the running unit's.
type fakeEnvoyRegistry struct {
	mu    sync.Mutex
	other []string
}

func startFakeEnvoyRegistry(t *testing.T) (*fakeEnvoyRegistry, string) {
	t.Helper()
	f := &fakeEnvoyRegistry{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/registry/agent-live" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"session_id":"agent-live","state":"live","title":"Spec writer","last_seen":%d}`, time.Now().UnixMilli())
			return
		}
		f.mu.Lock()
		f.other = append(f.other, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func runIssueHolder(t *testing.T, configPath string, args ...string) cliRun {
	t.Helper()
	var stdout, stderr cliOutput
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code := runIssueHolderCommand(ctx, append([]string{"issue-holder"}, args...), configPath, &stdout, &stderr)
	return cliRun{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// TestIssueHolderPrintsTheToolsAnswer fails if issue-holder does not print,
// on one line, exactly what the Concierge's issue_holder tool answers with
// the unit's Dispatch token and Envoy registry (here a live session holder),
// or if it opens an Envoy session of its own, which would take the running
// unit's: through the unit's opener, or by any Envoy request but the
// registry read.
func TestIssueHolderPrintsTheToolsAnswer(t *testing.T) {
	opened := make(chan struct{}, 1)
	original := openEnvoySession
	t.Cleanup(func() { openEnvoySession = original })
	openEnvoySession = func(context.Context, *envoy.Listener, *envoy.SessionConfig) (envoySession, error) {
		select {
		case opened <- struct{}{}:
		default:
		}
		return nil, errors.New("no Envoy session in this test")
	}
	token := strings.Repeat("t", 12)
	dispatchServer := startFakeDispatch(t, token, "")
	registry, envoyURL := startFakeEnvoyRegistry(t)
	configPath := firstAudioUnit(t, fmt.Sprintf("concierge:\n  dispatch_url: %s\n  envoy_url: %s\n", dispatchServer.URL, envoyURL))
	t.Setenv("GHOST_WISPR_DISPATCH_TOKEN", token)
	run := runIssueHolder(t, configPath, "abc-12")
	if run.code != 0 {
		t.Fatalf("exit %d, stderr %q", run.code, run.stderr)
	}
	if want := `{"holder":"session","live":true,"session":"agent-live","title":"Spec writer"}` + "\n"; run.stdout != want {
		t.Fatalf("printed %q, want the tool's answer %q", run.stdout, want)
	}
	select {
	case <-opened:
		t.Fatal("issue-holder opened an Envoy session")
	case <-time.After(300 * time.Millisecond):
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.other) != 0 {
		t.Fatalf("issue-holder made Envoy requests %q besides reading the registry", registry.other)
	}
}

// dirState is each file in dir but SQLite's shared-memory index, which a
// reader writes: its size, modification time and SHA-256.
func dirState(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "-shm") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		var sum [sha256.Size]byte
		if !entry.IsDir() {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			sum = sha256.Sum256(data)
		}
		state[entry.Name()] = fmt.Sprintf("%d bytes, modified %s, sha256 %x", info.Size(), info.ModTime().Format(time.RFC3339Nano), sum)
	}
	return state
}

// TestIssueHolderReadsTheUnitsSessionAndLeavesItsDatabaseAlone fails if
// issue-holder, run beside the running unit, does not take the unit's own
// Envoy session id from the unit's database (an issue nobody claims is held
// by the newest session that wrote to it other than the unit), or changes
// anything in the unit's data directory: opening the database as the unit
// does backs it up, restores and erases the unit's files, and would make the
// unit an Envoy session id if it had none.
func TestIssueHolderReadsTheUnitsSessionAndLeavesItsDatabaseAlone(t *testing.T) {
	configPath := firstAudioUnit(t, "")
	unit, err := storage.NewSQLiteStore(filepath.Join(filepath.Dir(configPath), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unit.Close() })
	state, err := concierge.LoadEnvoyState(unit)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("t", 12)
	dispatchServer := startFakeDispatch(t, token, state.SessionID)
	registry, envoyURL := startFakeEnvoyRegistry(t)
	t.Setenv("GHOST_WISPR_CONCIERGE_DISPATCH_URL", dispatchServer.URL)
	t.Setenv("GHOST_WISPR_CONCIERGE_ENVOY_URL", envoyURL)
	t.Setenv("GHOST_WISPR_DISPATCH_TOKEN", token)
	before := dirState(t, filepath.Dir(configPath))

	run := runIssueHolder(t, configPath, "ABC-13")
	if run.code != 0 {
		t.Fatalf("exit %d, stderr %q", run.code, run.stderr)
	}
	if want := `{"holder":"session","live":true,"session":"agent-live","title":"Spec writer"}` + "\n"; run.stdout != want {
		t.Fatalf("printed %q, want agent-live, the newest writer but the unit (%s): %q", run.stdout, state.SessionID, want)
	}
	after := dirState(t, filepath.Dir(configPath))
	for name, was := range before {
		if now, ok := after[name]; !ok || now != was {
			t.Errorf("the unit's %s was %s; after issue-holder it is %q", name, was, now)
		}
	}
	for name, now := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("issue-holder left %s (%s) in the unit's data directory", name, now)
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.other) != 0 {
		t.Fatalf("issue-holder made Envoy requests %q besides reading agent-live's liveness", registry.other)
	}
}

// TestIssueHolderGivesAUnitWithoutASessionIdNone fails if issue-holder, on a
// unit whose voice never opened an Envoy session, fails, or makes the unit a
// session id as the unit's own start does: the unit would then register
// under an id no one chose.
func TestIssueHolderGivesAUnitWithoutASessionIdNone(t *testing.T) {
	configPath := firstAudioUnit(t, "")
	unit, err := storage.NewSQLiteStore(filepath.Join(filepath.Dir(configPath), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unit.Close() })
	token := strings.Repeat("t", 12)
	dispatchServer := startFakeDispatch(t, token, "")
	_, envoyURL := startFakeEnvoyRegistry(t)
	t.Setenv("GHOST_WISPR_CONCIERGE_DISPATCH_URL", dispatchServer.URL)
	t.Setenv("GHOST_WISPR_CONCIERGE_ENVOY_URL", envoyURL)
	t.Setenv("GHOST_WISPR_DISPATCH_TOKEN", token)
	before := dirState(t, filepath.Dir(configPath))

	run := runIssueHolder(t, configPath, "ABC-12")
	if run.code != 0 || run.stdout != `{"holder":"session","live":true,"session":"agent-live","title":"Spec writer"}`+"\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and agent-live", run.code, run.stdout, run.stderr)
	}
	if after := dirState(t, filepath.Dir(configPath)); !maps.Equal(after, before) {
		t.Fatalf("the unit's data directory was %q; after issue-holder it is %q", before, after)
	}
}

// TestIssueHolderRefusedTokenExitsOne fails if a token Dispatch refuses
// leaves issue-holder exiting 0, printing an answer, or without Dispatch's
// refusal in the first 300 bytes of stderr, all accept-voice.sh keeps: it
// reads a refused read as a FAIL, never as an issue nobody holds.
func TestIssueHolderRefusedTokenExitsOne(t *testing.T) {
	dispatchServer := startFakeDispatch(t, strings.Repeat("t", 12), "")
	configPath := firstAudioUnit(t, fmt.Sprintf("concierge:\n  dispatch_url: %s\n", dispatchServer.URL))
	t.Setenv("GHOST_WISPR_DISPATCH_TOKEN", strings.Repeat("w", 12))
	run := runIssueHolder(t, configPath, "ABC-12")
	kept := run.stderr[:min(300, len(run.stderr))]
	if run.code != 1 || run.stdout != "" || !strings.Contains(kept, "answered 401") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 1, nothing printed, and Dispatch's 401 in stderr's first 300 bytes", run.code, run.stdout, run.stderr)
	}
}

// TestIssueHolderUsage fails if a wrong argument count does not exit 2 with
// the usage on stderr, or answers with the light command's usage.
func TestIssueHolderUsage(t *testing.T) {
	configPath := firstAudioUnit(t, "")
	for name, args := range map[string][]string{"no key": nil, "two keys": {"ABC-12", "ABC-13"}} {
		t.Run(name, func(t *testing.T) {
			run := runIssueHolder(t, configPath, args...)
			if run.code != 2 || !strings.Contains(run.stderr, "usage: ghost-wispr issue-holder KEY") || strings.Contains(run.stderr, "light-off") || run.stdout != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 2 and the usage", run.code, run.stdout, run.stderr)
			}
		})
	}
}
