package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// staticToken is a token source that always gives test-token.
type staticToken struct{}

func (staticToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "test-token", Type: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
}

func testCredentials() *auth.Credentials {
	return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken{}})
}

// sseAudio is one server-sent event of Vertex AI's streamGenerateContent
// carrying pcm, as Gemini TTS streams it: headerless PCM16, base64.
func sseAudio(pcm []byte) string {
	return `data: {"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` +
		base64.StdEncoding.EncodeToString(pcm) + `"}}]}}]}` + "\r\n\r\n"
}

// recordingTransport answers every request with one SSE event of pcm and
// keeps the last request.
type recordingTransport struct {
	pcm  []byte
	mu   sync.Mutex
	req  *http.Request
	body []byte
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	rt.mu.Lock()
	rt.req, rt.body = r, body
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sseAudio(rt.pcm))),
		Request:    r,
	}, nil
}

// TestGeminiPlaysTheFirstAudioBeforeTheStreamEnds fails if voice A on Gemini
// waits for the whole synthesis before it writes the first frame: the fake
// Vertex AI holds the rest of the stream until the first frame is written.
func TestGeminiPlaysTheFirstAudioBeforeTheStreamEnds(t *testing.T) {
	first, rest := ramp(0, 1501), ramp(40, 1999)
	out := newFakeOutput()
	var endedFirst atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStream(r) {
			http.NotFound(w, r) // the Session's warm-up, answered at once as Vertex AI does
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseAudio(first))
		w.(http.Flusher).Flush()
		select {
		case <-out.first:
		case <-time.After(2 * time.Second):
			endedFirst.Store(true)
		}
		_, _ = io.WriteString(w, sseAudio(rest))
	}))
	t.Cleanup(srv.Close)
	g := newTestGemini(t, srv.URL, nil)
	s := openSession(t, g, out)

	if err := s.Say(context.Background(), "Twelve open issues, three blocked.", voice.Fact); err != nil {
		t.Fatalf("Say: %v", err)
	}
	if endedFirst.Load() {
		t.Fatal("the first frame was written only after the stream ended, want it written as the first chunk arrives")
	}
	want := append(bytes.Clone(first), rest...)
	if got := out.waitPlayback(t, 1, len(want))[0].audio(); !bytes.Equal(got, want) {
		t.Fatalf("played %d bytes, want the stream's %d in order", len(got), len(want))
	}
}

// TestGeminiRejectsNoAudioWithItsFinishReason fails if a streaming Gemini
// response with no inline audio reaches voice A as a successful Say, or hides
// the finish reason that explains why it supplied none.
func TestGeminiRejectsNoAudioWithItsFinishReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStream(r) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"candidates":[{"finishReason":"SAFETY","content":{"parts":[]}}]}`+"\n\n")
	}))
	t.Cleanup(srv.Close)
	g := newTestGemini(t, srv.URL, nil)
	err := g.Stream(context.Background(), "A sentence.", func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "the engine gave no audio") || !strings.Contains(err.Error(), "SAFETY") {
		t.Fatalf("Stream error = %v, want no audio and SAFETY", err)
	}
}

// TestGeminiRequestsStreamingSpeech fails if the engine of a Vertex AI
// provider does not ask the Vertex AI host of the provider's location (a
// multi-region's own host, the global host, or a region's) for the model's
// streamed audio in the voice it was given ("Kore" when none), signed in with
// the provider's Google token, or does not emit the audio it streams back.
func TestGeminiRequestsStreamingSpeech(t *testing.T) {
	for _, tc := range []struct {
		name, location, model, voice string
		wantURL, wantVoice           string
	}{{
		name: "us", location: "us", model: "gemini-3.8-flash-tts",
		wantURL:   "https://aiplatform.us.rep.googleapis.com/v1/projects/example-project/locations/us/publishers/google/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse",
		wantVoice: "Kore",
	}, {
		name: "named", location: "eu", model: "gemini-3.8-flash-lite-tts", voice: "Puck",
		wantURL:   "https://aiplatform.eu.rep.googleapis.com/v1/projects/example-project/locations/eu/publishers/google/models/gemini-3.8-flash-lite-tts:streamGenerateContent?alt=sse",
		wantVoice: "Puck",
	}, {
		name: "global", location: "global", model: "gemini-3.8-flash-tts",
		wantURL:   "https://aiplatform.googleapis.com/v1/projects/example-project/locations/global/publishers/google/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse",
		wantVoice: "Kore",
	}, {
		name: "no location is global", model: "gemini-3.8-flash-tts",
		wantURL:   "https://aiplatform.googleapis.com/v1/projects/example-project/locations/global/publishers/google/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse",
		wantVoice: "Kore",
	}, {
		name: "region", location: "us-central1", model: "gemini-3.8-flash-tts",
		wantURL:   "https://us-central1-aiplatform.googleapis.com/v1/projects/example-project/locations/us-central1/publishers/google/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse",
		wantVoice: "Kore",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &recordingTransport{pcm: ramp(0, 2400)}
			p := vertexAt("")
			p.Location = tc.location
			g, err := NewEngine(testProvider(t, p, nil), tc.model, tc.voice, WithHTTPClient(&http.Client{Transport: rt}))
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			if g.Format() != mono24k {
				t.Fatalf("Format = %+v, want PCM16 mono at 24 kHz", g.Format())
			}
			var got []byte
			if err := g.Stream(context.Background(), "Twelve open issues.", func(pcm []byte) error {
				got = append(got, pcm...)
				return nil
			}); err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if !bytes.Equal(got, rt.pcm) {
				t.Fatalf("emitted %d bytes, want the streamed %d", len(got), len(rt.pcm))
			}
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if rt.req == nil {
				t.Fatal("no request reached Vertex AI")
			}
			if got := rt.req.URL.String(); got != tc.wantURL {
				t.Errorf("request URL = %s, want %s", got, tc.wantURL)
			}
			if got := rt.req.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("Authorization = %q, want the provider's Google token", got)
			}
			text, voice := geminiRequestTextAndVoice(t, rt.body)
			if text != "Twelve open issues." {
				t.Errorf("request text = %q, want the text as one part", text)
			}
			if voice != tc.wantVoice {
				t.Errorf("speechConfig.voiceConfig.voice = %q, want %q", voice, tc.wantVoice)
			}
		})
	}
}

// geminiRequestTextAndVoice reads a streamGenerateContent request: its one
// text part and its voice. It fails the test unless the request asks for
// audio alone.
func geminiRequestTextAndVoice(t *testing.T, raw []byte) (text, voice string) {
	t.Helper()
	var body struct {
		Contents []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			ResponseModalities []string `json:"responseModalities"`
			SpeechConfig       struct {
				VoiceConfig struct {
					Voice string `json:"voice"`
				} `json:"voiceConfig"`
			} `json:"speechConfig"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("request body %s: %v", raw, err)
	}
	if m := body.GenerationConfig.ResponseModalities; len(m) != 1 || m[0] != "AUDIO" {
		t.Errorf("responseModalities = %q, want [AUDIO]", m)
	}
	if len(body.Contents) != 1 || len(body.Contents[0].Parts) != 1 {
		t.Errorf("request contents = %s, want one part", raw)
		return "", body.GenerationConfig.SpeechConfig.VoiceConfig.Voice
	}
	return body.Contents[0].Parts[0].Text, body.GenerationConfig.SpeechConfig.VoiceConfig.Voice
}

// TestGeminiAPIEngineStreamsFromGooglesGeminiAPI fails if the engine of a
// Gemini API provider does not ask {url}/v1beta/models/{model} for streamed
// audio, signed in with the provider's key in X-Goog-Api-Key, in the same
// request shape as Vertex AI's, or does not emit the audio streamed back.
func TestGeminiAPIEngineStreamsFromGooglesGeminiAPI(t *testing.T) {
	pcm := ramp(3, 1920)
	var gotPath, gotQuery, gotKey string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStream(r) {
			http.NotFound(w, r)
			return
		}
		gotPath, gotQuery, gotKey = r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Goog-Api-Key")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseAudio(pcm))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, &config.Provider{API: config.APIGemini, URL: srv.URL, Auth: keyAuth()}, nil)
	g, err := NewEngine(p, "gemini-3.8-flash-tts", "Puck")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	var got []byte
	if err := g.Stream(context.Background(), "Three are blocked.", func(b []byte) error {
		got = append(got, b...)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if gotPath != "/v1beta/models/gemini-3.8-flash-tts:streamGenerateContent" || gotQuery != "alt=sse" {
		t.Errorf("request %s?%s, want /v1beta/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse", gotPath, gotQuery)
	}
	if gotKey != testKey {
		t.Errorf("X-Goog-Api-Key = %q, want the provider's key", gotKey)
	}
	if text, voice := geminiRequestTextAndVoice(t, gotBody); text != "Three are blocked." || voice != "Puck" {
		t.Errorf("request text %q in voice %q, want the text in Puck", text, voice)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("emitted %d bytes, want the streamed %d", len(got), len(pcm))
	}
}

// TestGeminiAPIEngineSpeaksThroughAGateway fails if the engine of a Gemini
// API provider at a model gateway, signed in with a Google ID token
// (google_id_token), does not ask {url}/v1beta/models/{model} for streamed
// audio with Authorization: Bearer <the ID token>, sends any key header
// beside it, or does not emit the audio streamed back.
func TestGeminiAPIEngineSpeaksThroughAGateway(t *testing.T) {
	const idToken = "eyJhbGciOiJub25lIn0.eyJleHAiOjQxMDI0NDQ4MDB9.sig"
	pcm := ramp(3, 1920)
	var gotPath, gotQuery, gotAuth, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStream(r) {
			http.NotFound(w, r)
			return
		}
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAuth, gotKey = r.Header.Get("Authorization"), r.Header.Get("X-Goog-Api-Key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseAudio(pcm))
	}))
	t.Cleanup(srv.Close)
	set := providers.New(&config.Config{Providers: map[string]config.Provider{
		"gateway-gemini": {API: config.APIGemini, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthGoogleIDToken, Audience: "model-gateway-example"}},
	}}, providers.Options{IDToken: func(context.Context, string, string) (string, error) { return idToken, nil }})
	p, err := set.Get("gateway-gemini")
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewEngine(p, "gemini-3.8-flash-tts", "Kore")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	var got []byte
	if err := g.Stream(context.Background(), "Three are blocked.", func(b []byte) error {
		got = append(got, b...)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if gotPath != "/v1beta/models/gemini-3.8-flash-tts:streamGenerateContent" || gotQuery != "alt=sse" {
		t.Errorf("request %s?%s, want /v1beta/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse", gotPath, gotQuery)
	}
	if gotAuth != "Bearer "+idToken || gotKey != "" {
		t.Errorf("Authorization %q, X-Goog-Api-Key %q; want the ID token as a bearer token and no key", gotAuth, gotKey)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("emitted %d bytes, want the streamed %d", len(got), len(pcm))
	}
}

// geminiPlanted stands for a meeting's words in what Vertex AI answers.
const geminiPlanted = "quokkaplantedroomword"

// sseAudioNaming is sseAudio's event naming modelVersion ("" names none), as
// each event of Vertex AI's and the Gemini API's stream does.
func sseAudioNaming(t *testing.T, pcm []byte, modelVersion string) string {
	t.Helper()
	inline := map[string]any{"mimeType": "audio/L16;codec=pcm;rate=24000", "data": pcm}
	event := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"inlineData": inline}}}}}}
	if modelVersion != "" {
		event["modelVersion"] = modelVersion
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(data) + "\r\n\r\n"
}

// TestGeminiReturnsTheModelItsAnswerNames fails if the engine of a Vertex AI
// or Gemini API provider does not return the model its answer names in
// modelVersion, which can differ from the model it asked for, or returns a
// modelVersion that is no plain token as anything but "unknown": the
// answer's text can be anything, and the model reaches the log and POST
// /api/tts/speak's status.
func TestGeminiReturnsTheModelItsAnswerNames(t *testing.T) {
	pcm := ramp(0, 960)
	for modelVersion, want := range map[string]string{
		"gemini-3.8-flash-tts-001":       "gemini-3.8-flash-tts-001",
		"":                               "",
		"the room said " + geminiPlanted: "unknown",
		geminiPlanted + "\nsecond line":  "unknown",
		strings.Repeat("x", 80):          "unknown",
	} {
		stream := sseAudioNaming(t, pcm[:480], modelVersion) + sseAudioNaming(t, pcm[480:], modelVersion)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isStream(r) {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, stream)
		}))
		for api, p := range map[string]*config.Provider{
			config.APIVertex: vertexAt(srv.URL),
			config.APIGemini: {API: config.APIGemini, URL: srv.URL, Auth: keyAuth()},
		} {
			e, err := NewEngine(testProvider(t, p, nil), "gemini-3.8-flash-tts", "")
			if err != nil {
				t.Fatalf("%s: NewEngine: %v", api, err)
			}
			reporter, ok := e.(ModelReporter)
			if !ok {
				t.Fatalf("%s: the engine %T does not report the model its answer names", api, e)
			}
			var got []byte
			model, err := reporter.StreamModel(context.Background(), "hello", func(b []byte) error {
				got = append(got, b...)
				return nil
			})
			if err != nil {
				t.Fatalf("%s, modelVersion %q: StreamModel: %v", api, modelVersion, err)
			}
			if model != want {
				t.Errorf("%s, modelVersion %q: model %q, want %q", api, modelVersion, model, want)
			}
			if !bytes.Equal(got, pcm) {
				t.Errorf("%s: emitted %d bytes, want the streamed %d", api, len(got), len(pcm))
			}
		}
		srv.Close()
	}
}

// TestGeminiFailsTheStreamOnAnAPIError fails if a refused request does not
// fail the Stream with Vertex AI's status and code, or keeps its message,
// which can quote the text asked for, or the body.
func TestGeminiFailsTheStreamOnAnAPIError(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":403,"message":"Permission denied for ` + geminiPlanted + `","status":"PERMISSION_DENIED"}}`,
		`not JSON: ` + geminiPlanted,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, body)
		}))
		g := newTestGemini(t, srv.URL, nil)
		err := g.Stream(context.Background(), "hello", func([]byte) error { return nil })
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), geminiPlanted) {
			t.Errorf("Stream = %v, want Vertex AI's status and not its text, for body %s", err, body)
		}
		if strings.HasPrefix(body, "{") && (err == nil || !strings.Contains(err.Error(), "PERMISSION_DENIED")) {
			t.Errorf("Stream = %v, want Vertex AI's code PERMISSION_DENIED", err)
		}
	}
}

// TestGeminiFailsTheStreamOnAnErrorEvent fails if an error Vertex AI sends
// inside the stream, after its 200, does not fail the Stream with its status
// and code, or keeps its message.
func TestGeminiFailsTheStreamOnAnErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseAudio(ramp(0, 960)))
		_, _ = io.WriteString(w, `data: {"error":{"code":429,"message":"Resource exhausted by `+geminiPlanted+`","status":"RESOURCE_EXHAUSTED"}}`+"\r\n\r\n")
	}))
	t.Cleanup(srv.Close)
	g := newTestGemini(t, srv.URL, nil)
	emitted := 0
	err := g.Stream(context.Background(), "hello", func(pcm []byte) error {
		emitted += len(pcm)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "RESOURCE_EXHAUSTED") || strings.Contains(err.Error(), geminiPlanted) {
		t.Fatalf("Stream = %v, want Vertex AI's status and code, not its message", err)
	}
	if emitted != 960 {
		t.Fatalf("emitted %d bytes before the error, want the 960 sent before it", emitted)
	}
}

// TestGeminiLogsNothingWhenInterrupted fails if stopping a Gemini stream that
// waits for its next event writes to the process log, as every barge-in
// would.
func TestGeminiLogsNothingWhenInterrupted(t *testing.T) {
	var logged lockedBuffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseAudio(ramp(0, 960)))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	g := newTestGemini(t, srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := g.Stream(ctx, "hello", func([]byte) error {
		cancel() // the barge-in, while the stream waits for its next event
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted Stream = %v, want context.Canceled", err)
	}
	if got := logged.String(); got != "" {
		t.Fatalf("an interrupted Stream wrote %q to the process log, want nothing", got)
	}
}

// lockedBuffer is a bytes.Buffer safe for the log's writes and the test's read.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// cannedTransport answers every request with the same SSE body.
type cannedTransport struct{ body string }

func (c cannedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_ = r.Body.Close()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Request:    r,
	}, nil
}

// BenchmarkGeminiStream measures decoding a 5-second answer streamed as 50
// events of 100 ms each.
func BenchmarkGeminiStream(b *testing.B) {
	var body strings.Builder
	for range 50 {
		body.WriteString(sseAudio(ramp(0, 4800)))
	}
	g, err := NewEngine(testProvider(b, vertexAt(""), nil), "gemini-3.8-flash-tts", "",
		WithHTTPClient(&http.Client{Transport: cannedTransport{body: body.String()}}))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := g.Stream(context.Background(), "hello", func([]byte) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

// countingToken is a token source that counts its calls.
type countingToken struct{ calls atomic.Int32 }

func (c *countingToken) Token(context.Context) (*auth.Token, error) {
	c.calls.Add(1)
	return &auth.Token{Value: "test-token", Type: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
}

// failingToken is a token source that always fails.
type failingToken struct{}

func (failingToken) Token(context.Context) (*auth.Token, error) {
	return nil, errors.New("no token today")
}

// isStream reports whether r asks for streamed speech, not a warm-up.
func isStream(r *http.Request) bool {
	return strings.HasSuffix(r.URL.Path, ":streamGenerateContent")
}

// TestGeminiWarmsItsConnectionWhenTheSessionOpens fails if a Gemini Session
// does not, as it opens, fetch a token and open a connection to Vertex AI
// that its first Say then uses, so that Say pays neither: the Supervisor
// opens the voice at the wake, a second or two before it speaks.
func TestGeminiWarmsItsConnectionWhenTheSessionOpens(t *testing.T) {
	var conns atomic.Int32
	warmed := make(chan struct{}, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStream(r) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseAudio(ramp(0, 960)))
			return
		}
		warmed <- struct{}{}
		http.NotFound(w, r)
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	tokens := &countingToken{}
	g := newTestGemini(t, srv.URL, auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: tokens}), WithHTTPClient(srv.Client()))
	s := openSession(t, g, newFakeOutput())
	select {
	case <-warmed:
	case <-time.After(2 * time.Second):
		t.Fatal("no warm-up request reached Vertex AI 2 s after Open")
	}
	deadline := time.Now().Add(2 * time.Second)
	for tokens.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Open fetched no token in 2 s, want it ready for the first Say")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Say(context.Background(), "hello", voice.Fact); err != nil {
		t.Fatalf("Say: %v", err)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("Vertex AI saw %d connections, want the first Say on the one the warm-up opened", n)
	}
}

// TestGeminiOpensWhenTheWarmUpFails fails if a warm-up that cannot reach
// Vertex AI, or get a token, fails Open: the Say would only dial itself.
func TestGeminiOpensWhenTheWarmUpFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := "http://" + ln.Addr().String()
	_ = ln.Close()
	g := newTestGemini(t, gone, auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: failingToken{}}))
	s, err := New(g).Open(context.Background(), &voice.Exchange{Out: newFakeOutput()})
	if err != nil {
		t.Fatalf("Open = %v, want the Session even though its warm-up fails", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestGeminiCloseStopsTheWarmUp fails if closing a Session leaves its
// warm-up running, or waits for a warm-up Vertex AI does not answer.
func TestGeminiCloseStopsTheWarmUp(t *testing.T) {
	warmed, ended := make(chan struct{}, 1), make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStream(r) {
			http.Error(w, "not in this test", http.StatusBadRequest)
			return
		}
		warmed <- struct{}{}
		select {
		case <-r.Context().Done():
			ended <- struct{}{}
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	g := newTestGemini(t, srv.URL, nil)
	s, err := New(g).Open(context.Background(), &voice.Exchange{Out: newFakeOutput()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	select {
	case <-warmed:
	case <-time.After(2 * time.Second):
		t.Fatal("no warm-up request reached Vertex AI 2 s after Open")
	}
	closed := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if took := time.Since(closed); took > time.Second {
		t.Fatalf("Close took %v with a warm-up unanswered, want it to stop the warm-up", took)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("the warm-up request was still open 1 s after Close")
	}
}

// shortStall shortens the stall timeout for one test.
func shortStall(t *testing.T) {
	t.Helper()
	stall := stallTimeout
	stallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { stallTimeout = stall })
}

// TestGeminiStallFailsTheStream fails if a Gemini stream that sends nothing
// for the stall timeout, before its answer, before its first event or between
// two, holds the Say, and with it the exchange's floor, instead of failing
// it with an error that says the stream stalled.
func TestGeminiStallFailsTheStream(t *testing.T) {
	shortStall(t)
	for _, tc := range []struct {
		name          string
		answer, event bool
		wantEmitted   int
	}{
		{name: "no answer"},
		{name: "an answer with no event", answer: true},
		{name: "after an event", answer: true, event: true, wantEmitted: 960},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// A read body lets the server see the client hang up.
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.answer {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
				}
				if tc.event {
					_, _ = io.WriteString(w, sseAudio(ramp(0, 960)))
				}
				if tc.answer {
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			}))
			t.Cleanup(srv.Close)
			g := newTestGemini(t, srv.URL, nil)
			emitted := 0
			done := make(chan error, 1)
			go func() {
				done <- g.Stream(context.Background(), "hello", func(pcm []byte) error {
					emitted += len(pcm)
					return nil
				})
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "no stream data") {
					t.Fatalf("stalled Stream = %v, want an error saying no stream data came", err)
				}
				if emitted != tc.wantEmitted {
					t.Fatalf("emitted %d bytes before the stall, want %d", emitted, tc.wantEmitted)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Stream still waiting 3 s on a stream that sends nothing")
			}
		})
	}
}

// TestGeminiSlowPlaybackIsNoStall fails if the time a Stream waits on the
// Output, which holds a voice that synthesises faster than real time back,
// counts toward the stall timeout.
func TestGeminiSlowPlaybackIsNoStall(t *testing.T) {
	shortStall(t)
	// The second event comes while the Stream still waits on the Output for
	// the first, and the stream stays open past both waits.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseAudio(ramp(0, 960)))
		w.(http.Flusher).Flush()
		time.Sleep(stallTimeout / 2)
		_, _ = io.WriteString(w, sseAudio(ramp(5, 960)))
		w.(http.Flusher).Flush()
		time.Sleep(3 * stallTimeout)
	}))
	t.Cleanup(srv.Close)
	g := newTestGemini(t, srv.URL, nil)
	emitted := 0
	if err := g.Stream(context.Background(), "hello", func(pcm []byte) error {
		time.Sleep(3 * stallTimeout) // the Output holds 200 ms and plays on
		emitted += len(pcm)
		return nil
	}); err != nil {
		t.Fatalf("Stream held back by playback = %v, want nil", err)
	}
	if emitted != 2*960 {
		t.Fatalf("emitted %d bytes, want both events' %d", emitted, 2*960)
	}
}

// TestNewEngineRequiresAVertexProject fails if an engine can be made for a
// Vertex AI provider that names no project, which no request can reach.
func TestNewEngineRequiresAVertexProject(t *testing.T) {
	p := vertexAt("")
	p.Project = ""
	if _, err := NewEngine(testProvider(t, p, nil), "gemini-3.8-flash-tts", ""); err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("NewEngine without a project = %v, want an error naming the project", err)
	}
}
