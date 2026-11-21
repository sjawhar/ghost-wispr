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
	g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewGemini: %v", err)
	}
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
	g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewGemini: %v", err)
	}
	err = g.Stream(context.Background(), "A sentence.", func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "the engine gave no audio") || !strings.Contains(err.Error(), "SAFETY") {
		t.Fatalf("Stream error = %v, want no audio and SAFETY", err)
	}
}

// TestGeminiRequestsStreamingSpeech fails if the Gemini engine does not ask
// the Vertex AI host of its location (a multi-region's own host, the global
// host, or a region's) for streamed audio in the voice it was given,
// authorised with the credentials' token, or does not emit the audio it
// streams back.
func TestGeminiRequestsStreamingSpeech(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     GeminiConfig
		wantURL string
		voice   string
	}{{
		name:    "defaults",
		cfg:     GeminiConfig{Project: "example-project"},
		wantURL: "https://aiplatform.us.rep.googleapis.com/v1/projects/example-project/locations/us/publishers/google/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse",
		voice:   "Kore",
	}, {
		name:    "named",
		cfg:     GeminiConfig{Project: "example-project", Location: "eu", Model: "gemini-3.8-flash-lite-tts", Voice: "Puck"},
		wantURL: "https://aiplatform.eu.rep.googleapis.com/v1/projects/example-project/locations/eu/publishers/google/models/gemini-3.8-flash-lite-tts:streamGenerateContent?alt=sse",
		voice:   "Puck",
	}, {
		name:    "global",
		cfg:     GeminiConfig{Project: "example-project", Location: "global"},
		wantURL: "https://aiplatform.googleapis.com/v1/projects/example-project/locations/global/publishers/google/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse",
		voice:   "Kore",
	}, {
		name:    "region",
		cfg:     GeminiConfig{Project: "example-project", Location: "us-central1"},
		wantURL: "https://us-central1-aiplatform.googleapis.com/v1/projects/example-project/locations/us-central1/publishers/google/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse",
		voice:   "Kore",
	}, {
		// Through the model gateway (GatewayURL): Google's own path, the model
		// in it, no project and no location; the gateway calls Vertex AI.
		name:    "gateway",
		cfg:     GeminiConfig{GatewayURL: "https://model-gateway.example.test"},
		wantURL: "https://model-gateway.example.test/v1beta/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse",
		voice:   "Kore",
	}, {
		name:    "gateway, named, a trailing slash",
		cfg:     GeminiConfig{GatewayURL: "https://model-gateway.example.test/", Model: "gemini-3.8-flash-tts", Voice: "Puck", Project: "ignored", Location: "eu"},
		wantURL: "https://model-gateway.example.test/v1beta/models/gemini-3.8-flash-tts:streamGenerateContent?alt=sse",
		voice:   "Puck",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &recordingTransport{pcm: ramp(0, 2400)}
			tc.cfg.Credentials = testCredentials()
			tc.cfg.HTTPClient = &http.Client{Transport: rt}
			g, err := NewGemini(&tc.cfg)
			if err != nil {
				t.Fatalf("NewGemini: %v", err)
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
				t.Errorf("Authorization = %q, want the credentials' bearer token", got)
			}
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
			if err := json.Unmarshal(rt.body, &body); err != nil {
				t.Fatalf("request body %s: %v", rt.body, err)
			}
			if len(body.Contents) != 1 || len(body.Contents[0].Parts) != 1 || body.Contents[0].Parts[0].Text != "Twelve open issues." {
				t.Errorf("request contents = %s, want the text as one part", rt.body)
			}
			if m := body.GenerationConfig.ResponseModalities; len(m) != 1 || m[0] != "AUDIO" {
				t.Errorf("responseModalities = %q, want [AUDIO]", m)
			}
			if v := body.GenerationConfig.SpeechConfig.VoiceConfig.Voice; v != tc.voice {
				t.Errorf("speechConfig.voiceConfig.voice = %q, want %q", v, tc.voice)
			}
		})
	}
}

// geminiPlanted stands for a meeting's words in what Vertex AI answers.
const geminiPlanted = "quokkaplantedroomword"

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
		g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: srv.URL})
		if err != nil {
			t.Fatalf("NewGemini: %v", err)
		}
		err = g.Stream(context.Background(), "hello", func([]byte) error { return nil })
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
	g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewGemini: %v", err)
	}
	emitted := 0
	err = g.Stream(context.Background(), "hello", func(pcm []byte) error {
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
	g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewGemini: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = g.Stream(ctx, "hello", func([]byte) error {
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
	g, err := NewGemini(&GeminiConfig{
		Project:     "example-project",
		Credentials: testCredentials(),
		HTTPClient:  &http.Client{Transport: cannedTransport{body: body.String()}},
	})
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
	g, err := NewGemini(&GeminiConfig{
		Project:     "example-project",
		Credentials: auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: tokens}),
		BaseURL:     srv.URL,
		HTTPClient:  srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewGemini: %v", err)
	}
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
	g, err := NewGemini(&GeminiConfig{
		Project:     "example-project",
		Credentials: auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: failingToken{}}),
		BaseURL:     gone,
	})
	if err != nil {
		t.Fatalf("NewGemini: %v", err)
	}
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
	g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewGemini: %v", err)
	}
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

// shortGeminiStall shortens the stall timeout for one test.
func shortGeminiStall(t *testing.T) {
	t.Helper()
	stall := geminiStallTimeout
	geminiStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { geminiStallTimeout = stall })
}

// TestGeminiStallFailsTheStream fails if a Gemini stream that sends nothing
// for the stall timeout, before its answer, before its first event or between
// two, holds the Say, and with it the exchange's floor, instead of failing
// it with an error that says the stream stalled.
func TestGeminiStallFailsTheStream(t *testing.T) {
	shortGeminiStall(t)
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
			g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: srv.URL})
			if err != nil {
				t.Fatalf("NewGemini: %v", err)
			}
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
	shortGeminiStall(t)
	// The second event comes while the Stream still waits on the Output for
	// the first, and the stream stays open past both waits.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseAudio(ramp(0, 960)))
		w.(http.Flusher).Flush()
		time.Sleep(geminiStallTimeout / 2)
		_, _ = io.WriteString(w, sseAudio(ramp(5, 960)))
		w.(http.Flusher).Flush()
		time.Sleep(3 * geminiStallTimeout)
	}))
	t.Cleanup(srv.Close)
	g, err := NewGemini(&GeminiConfig{Project: "example-project", Credentials: testCredentials(), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewGemini: %v", err)
	}
	emitted := 0
	if err := g.Stream(context.Background(), "hello", func(pcm []byte) error {
		time.Sleep(3 * geminiStallTimeout) // the Output holds 200 ms and plays on
		emitted += len(pcm)
		return nil
	}); err != nil {
		t.Fatalf("Stream held back by playback = %v, want nil", err)
	}
	if emitted != 2*960 {
		t.Fatalf("emitted %d bytes, want both events' %d", emitted, 2*960)
	}
}

// TestNewGeminiRequiresAProjectAndCredentials fails if a Gemini engine can be
// made that cannot reach Vertex AI.
func TestNewGeminiRequiresAProjectAndCredentials(t *testing.T) {
	if _, err := NewGemini(&GeminiConfig{Credentials: testCredentials()}); err == nil {
		t.Error("NewGemini without a project succeeded")
	}
	if _, err := NewGemini(&GeminiConfig{Project: "example-project"}); err == nil {
		t.Error("NewGemini without credentials succeeded")
	}
	if _, err := NewGemini(&GeminiConfig{GatewayURL: "https://model-gateway.example.test"}); err == nil {
		t.Error("NewGemini through the gateway without credentials succeeded")
	}
	if _, err := NewGemini(&GeminiConfig{GatewayURL: "https://model-gateway.example.test", Credentials: testCredentials()}); err != nil {
		t.Errorf("NewGemini through the gateway with no project: %v; the gateway needs none", err)
	}
}
