package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// fakeSpeak is Deepgram's /v1/speak WebSocket as a test sees it: it keeps each
// connection's query and Authorization header and every message the client
// sent, and answers through onMessage. Its url is the http:// URL a provider
// names, which the engine dials as ws://.
type fakeSpeak struct {
	url       string
	onMessage func(c *speakConn, typ, text string)

	mu      sync.Mutex
	queries []url.Values
	auths   []string
	got     []string // "Speak <text>", "Flush", "Clear", "Close", across connections in order
	ended   []chan struct{}
}

// speakConn is one connection to the fake, safe for concurrent writes.
type speakConn struct {
	n   int // 1 for the first connection
	ws  *websocket.Conn
	wmu sync.Mutex
}

func (c *speakConn) audio(pcm []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteMessage(websocket.BinaryMessage, pcm)
}

func (c *speakConn) send(msg string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, []byte(msg))
}

func (c *speakConn) closeWith(code int, reason string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = c.ws.Close()
}

func startSpeak(t *testing.T, onMessage func(c *speakConn, typ, text string)) *fakeSpeak {
	t.Helper()
	f := &fakeSpeak{onMessage: onMessage}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ended := make(chan struct{})
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.Query())
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.ended = append(f.ended, ended)
		n := len(f.queries)
		f.mu.Unlock()
		defer close(ended)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		c := &speakConn{n: n, ws: ws}
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var m struct{ Type, Text string }
			_ = json.Unmarshal(data, &m)
			entry := m.Type
			if m.Type == "Speak" {
				entry += " " + m.Text
			}
			f.mu.Lock()
			f.got = append(f.got, entry)
			f.mu.Unlock()
			if m.Type == "Close" {
				return
			}
			f.onMessage(c, m.Type, m.Text)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeSpeak) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func (f *fakeSpeak) connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *fakeSpeak) waitEnded(t *testing.T, n int) {
	t.Helper()
	f.mu.Lock()
	ended := f.ended[n-1]
	f.mu.Unlock()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatalf("connection %d still open 2 s later", n)
	}
}

// speakBack is a Deepgram that, on each Flush, sends the audio of the text
// spoken since the last one, in two messages split mid-sample, then Flushed.
func speakBack(audioOf map[string][]byte) func(c *speakConn, typ, text string) {
	var pending string
	seq := 0
	return func(c *speakConn, typ, text string) {
		switch typ {
		case "Speak":
			pending = text
		case "Flush":
			if pcm := audioOf[pending]; len(pcm) > 0 {
				half := len(pcm)/2 | 1
				_ = c.audio(pcm[:half])
				_ = c.audio(pcm[half:])
			}
			_ = c.send(fmt.Sprintf(`{"type":"Flushed","sequence_id":%d}`, seq))
			seq++
		case "Clear":
			_ = c.send(fmt.Sprintf(`{"type":"Cleared","sequence_id":%d}`, seq))
		}
	}
}

// deepgramAt is a Deepgram provider at url, signed in with testKey.
func deepgramAt(url string) *config.Provider {
	return &config.Provider{API: config.APIDeepgram, URL: url, Auth: keyAuth()}
}

// newTestAura is the Aura-2 engine of the fake's Deepgram, in Harmonia's
// voice.
func newTestAura(t *testing.T, f *fakeSpeak, opts ...Option) Engine {
	t.Helper()
	a, err := NewEngine(testProvider(t, deepgramAt(f.url), nil), "aura-2-harmonia-en", "", opts...)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return a
}

// playedAudio is every frame the Output was given, once play stopped writing
// (no new frame for 50 ms): a Say returns once its first frame is queued,
// before its audio is all written.
func playedAudio(out *fakeOutput) []byte {
	read := func() []byte {
		var all [][]byte
		for _, p := range out.begun() {
			all = append(all, p.audio())
		}
		return bytes.Join(all, nil)
	}
	got, deadline := read(), time.Now().Add(2*time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		next := read()
		if len(next) == len(got) {
			return next
		}
		got = next
	}
	return got
}

// TestAuraConnectsWhenTheSessionOpens fails if an Aura-2 Session dials only at
// its first Say, which then pays the dial before its first audio.
func TestAuraConnectsWhenTheSessionOpens(t *testing.T) {
	f := startSpeak(t, speakBack(nil))
	openSession(t, newTestAura(t, f), newFakeOutput())
	if n := f.connections(); n != 1 {
		t.Fatalf("Deepgram had %d connections after Open, want 1 before any Say", n)
	}
}

// TestAuraAsksForLinear16At24kHzOptedOutWithTheKey fails if the socket does
// not ask for headerless PCM16 at 24 kHz in the model's voice, does not
// keep the text out of Deepgram's model training (mip_opt_out=true), or does
// not present the provider's key as a Deepgram token.
func TestAuraAsksForLinear16At24kHzOptedOutWithTheKey(t *testing.T) {
	f := startSpeak(t, speakBack(nil))
	a := newTestAura(t, f)
	if a.Format() != mono24k {
		t.Fatalf("Format = %+v, want PCM16 mono at 24 kHz", a.Format())
	}
	openSession(t, a, newFakeOutput())
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		t.Fatal("no connection reached Deepgram")
	}
	q := f.queries[0]
	for key, want := range map[string]string{
		"model":       "aura-2-harmonia-en",
		"encoding":    "linear16",
		"sample_rate": "24000",
		"mip_opt_out": "true",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
	if f.auths[0] != "Token "+testKey {
		t.Errorf("Authorization = %q, want the provider's key as a Deepgram token", f.auths[0])
	}
}

// TestAuraSpeaksInTheVoiceItIsGiven fails if a voice given to an Aura engine
// does not replace the model on the socket: each Aura-2 voice is a model.
func TestAuraSpeaksInTheVoiceItIsGiven(t *testing.T) {
	f := startSpeak(t, speakBack(nil))
	a, err := NewEngine(testProvider(t, deepgramAt(f.url), nil), "aura-2-harmonia-en", "aura-2-thalia-en")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	openSession(t, a, newFakeOutput())
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) != 1 || f.queries[0].Get("model") != "aura-2-thalia-en" {
		t.Fatalf("queries %v, want one socket asking for model aura-2-thalia-en", f.queries)
	}
}

// TestAuraSendsSpeakAndFlushForEachSay fails if a Say does not send its text
// and a Flush on the Session's one socket, or does not play the audio
// Deepgram sends back.
func TestAuraSendsSpeakAndFlushForEachSay(t *testing.T) {
	audioOf := map[string][]byte{"Twelve open issues.": ramp(0, 2001), "Three are blocked.": ramp(90, 1501)}
	f := startSpeak(t, speakBack(audioOf))
	out := newFakeOutput()
	s := openSession(t, newTestAura(t, f), out)
	for _, text := range []string{"Twelve open issues.", "Three are blocked."} {
		if err := s.Say(context.Background(), text, voice.Fact); err != nil {
			t.Fatalf("Say %q: %v", text, err)
		}
	}
	want := []string{"Speak Twelve open issues.", "Flush", "Speak Three are blocked.", "Flush"}
	if got := f.received(); !slices.Equal(got, want) {
		t.Fatalf("Deepgram got %q, want %q", got, want)
	}
	if n := f.connections(); n != 1 {
		t.Fatalf("Deepgram had %d connections, want the Session's one", n)
	}
	// Each text's odd byte count leaves one byte that is not a whole sample.
	wantAudio := append(bytes.Clone(audioOf["Twelve open issues."][:2000]), audioOf["Three are blocked."][:1500]...)
	if got := playedAudio(out); !bytes.Equal(got, wantAudio) {
		t.Fatalf("played %d bytes, want the %d Deepgram sent", len(got), len(wantAudio))
	}
}

// TestAuraClearsOnInterruptAndTheNextSayHearsOnlyItsOwnAudio fails if an
// Interrupt does not send Clear, if the next Say's text reaches Deepgram
// before the Clear, or if audio of the cleared text that was already on the
// way plays in the next Say.
func TestAuraClearsOnInterruptAndTheNextSayHearsOnlyItsOwnAudio(t *testing.T) {
	after, stale := ramp(7, 1200), ramp(200, frame20ms)
	// streaming guards cleared, so no chunk of the long answer follows Cleared.
	var streaming sync.Mutex
	cleared := false
	var pending string
	f := startSpeak(t, func(c *speakConn, typ, text string) {
		switch {
		case typ == "Speak":
			pending = text
		case typ == "Flush" && pending == "after":
			_ = c.audio(after)
			_ = c.send(`{"type":"Flushed","sequence_id":1}`)
		case typ == "Flush":
			go func() {
				for {
					time.Sleep(10 * time.Millisecond)
					streaming.Lock()
					if cleared || c.audio(ramp(0, frame20ms)) != nil {
						streaming.Unlock()
						return
					}
					streaming.Unlock()
				}
			}()
		case typ == "Clear":
			streaming.Lock()
			cleared = true
			streaming.Unlock()
			_ = c.audio(stale)
			_ = c.send(`{"type":"Cleared","sequence_id":0}`)
		}
	})
	out := newFakeOutput()
	s := openSession(t, newTestAura(t, f), out)

	// The Say returns once its first frame is queued; the Interrupt then
	// clears the rest of its text at Deepgram.
	if err := s.Say(context.Background(), "a long answer", voice.Fact); err != nil {
		t.Fatalf("Say: %v", err)
	}
	out.waitFirstWrite(t)
	interrupted := time.Now()
	s.Interrupt()
	if took := time.Since(interrupted); took > 100*time.Millisecond {
		t.Fatalf("Interrupt took %v, want within 100 ms", took)
	}
	if err := s.Say(context.Background(), "after", voice.Fact); err != nil {
		t.Fatalf("Say after Interrupt: %v", err)
	}
	want := []string{"Speak a long answer", "Flush", "Clear", "Speak after", "Flush"}
	if got := f.received(); !slices.Equal(got, want) {
		t.Fatalf("Deepgram got %q, want %q", got, want)
	}
	pbs := out.waitPlayback(t, len(out.begun()), len(after))
	if got := pbs[len(pbs)-1].audio(); !bytes.Equal(got, after) {
		t.Fatalf("the Say after Interrupt played %d bytes, want only its own %d", len(got), len(after))
	}
}

// TestAuraCloseSendsCloseAndEndsTheConnection fails if closing an idle
// Session does not tell Deepgram with Close and end the socket.
func TestAuraCloseSendsCloseAndEndsTheConnection(t *testing.T) {
	f := startSpeak(t, speakBack(map[string][]byte{"hello": ramp(0, 1000)}))
	out := newFakeOutput()
	s := openSession(t, newTestAura(t, f), out)
	if err := s.Say(context.Background(), "hello", voice.Ack); err != nil {
		t.Fatalf("Say: %v", err)
	}
	// Say returns at its first frame; the Session is idle once the text has
	// all played.
	out.waitPlayback(t, 1, 1000)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.waitEnded(t, 1)
	if got := f.received(); !slices.Contains(got, "Close") {
		t.Fatalf("Deepgram got %q before the connection ended, want Close", got)
	}
}

// TestAuraRedialsWhenDeepgramClosedTheSocket fails if a Say after Deepgram
// closed the Session's socket fails instead of speaking on a new one.
func TestAuraRedialsWhenDeepgramClosedTheSocket(t *testing.T) {
	audioOf := map[string][]byte{"one": ramp(0, 1000), "two": ramp(50, 1000)}
	back := speakBack(audioOf)
	f := startSpeak(t, func(c *speakConn, typ, text string) {
		back(c, typ, text)
		if typ == "Flush" {
			c.closeWith(websocket.CloseNormalClosure, "")
		}
	})
	out := newFakeOutput()
	s := openSession(t, newTestAura(t, f), out)
	if err := s.Say(context.Background(), "one", voice.Fact); err != nil {
		t.Fatalf("Say: %v", err)
	}
	f.waitEnded(t, 1)
	if err := s.Say(context.Background(), "two", voice.Fact); err != nil {
		t.Fatalf("Say after Deepgram closed the socket: %v", err)
	}
	if n := f.connections(); n != 2 {
		t.Fatalf("Deepgram had %d connections, want a second after it closed the first", n)
	}
	if got := playedAudio(out); !bytes.Equal(got, append(bytes.Clone(audioOf["one"]), audioOf["two"]...)) {
		t.Fatalf("played %d bytes, want both Says' 2000", len(got))
	}
}

// TestAuraSaysTheTextAgainWhenTheSocketDiesBeforeAnyAudio fails if a Say
// whose socket Deepgram closes after the Speak, before any audio, fails
// instead of saying the text on a new socket: the socket looked open when
// the Say began.
func TestAuraSaysTheTextAgainWhenTheSocketDiesBeforeAnyAudio(t *testing.T) {
	pcm := ramp(0, 1000)
	back := speakBack(map[string][]byte{"hello": pcm})
	f := startSpeak(t, func(c *speakConn, typ, text string) {
		if c.n == 1 {
			c.closeWith(websocket.CloseGoingAway, "idle")
			return
		}
		back(c, typ, text)
	})
	out := newFakeOutput()
	s := openSession(t, newTestAura(t, f), out)
	if err := s.Say(context.Background(), "hello", voice.Fact); err != nil {
		t.Fatalf("Say: %v", err)
	}
	if n := f.connections(); n != 2 {
		t.Fatalf("Deepgram had %d connections, want a second for the text the first lost", n)
	}
	if got := playedAudio(out); !bytes.Equal(got, pcm) {
		t.Fatalf("played %d bytes, want the text's %d once", len(got), len(pcm))
	}
}

// TestAuraFailureFailsTheSay fails if a socket Deepgram ends with an error
// does not fail the Say and report it with Deepgram's close code, or keeps
// Deepgram's reason, which can quote the text spoken.
func TestAuraFailureFailsTheSay(t *testing.T) {
	f := startSpeak(t, func(c *speakConn, typ, _ string) {
		if typ == "Flush" {
			c.closeWith(websocket.CloseInternalServerErr, "synthesis unavailable for "+auraPlanted)
		}
	})
	s := openSession(t, newTestAura(t, f), newFakeOutput())
	err := s.Say(context.Background(), "hello", voice.Fact)
	if err == nil || !strings.Contains(err.Error(), "code 1011") || strings.Contains(err.Error(), "synthesis unavailable") {
		t.Fatalf("Say = %v, want Deepgram's close code and not its reason", err)
	}
	got := expectEvents(t, s.Events(), voice.EventAgentText, voice.EventErr)
	if got[1].Err == nil || !strings.Contains(got[1].Err.Error(), "code 1011") || strings.Contains(got[1].Err.Error(), auraPlanted) {
		t.Fatalf("EventErr = %v, want Deepgram's close code and not its reason", got[1].Err)
	}
}

// auraPlanted stands for a meeting's words in what Deepgram answers.
const auraPlanted = "quokkaplantedroomword"

// TestAuraStallFailsTheSayWithDeepgramsWarning fails if a Say waits for good
// on a Flush Deepgram never answers, or its error does not name the warning's
// code Deepgram sent instead, or keeps the warning's text.
func TestAuraStallFailsTheSayWithDeepgramsWarning(t *testing.T) {
	stall := auraStallTimeout
	auraStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { auraStallTimeout = stall })
	f := startSpeak(t, func(c *speakConn, typ, _ string) {
		if typ == "Flush" {
			_ = c.send(`{"type":"Warning","description":"Too many Flush messages for ` + auraPlanted + `","code":"FLUSH_LIMIT"}`)
		}
	})
	s := openSession(t, newTestAura(t, f), newFakeOutput())
	said := make(chan error, 1)
	go func() { said <- s.Say(context.Background(), "hello", voice.Fact) }()
	select {
	case err := <-said:
		if err == nil || !strings.Contains(err.Error(), "FLUSH_LIMIT") || strings.Contains(err.Error(), "Too many Flush") {
			t.Fatalf("Say = %v, want an error naming Deepgram's warning code and not its text", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Say still waiting 3 s on a Flush Deepgram never answered")
	}
}

// TestAuraRefusedHandshakeKeepsOnlyItsStatus fails if Deepgram's refusal of
// the socket keeps its status text or its dg-error header, which can quote
// the text asked for, instead of the status code alone; or is no
// providererr.Error of the provider's answer, named as the config names the
// provider, which the speak API's tts warning reads.
func TestAuraRefusedHandshakeKeepsOnlyItsStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("dg-error", "Could not speak "+auraPlanted)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	a, err := NewEngine(testProvider(t, deepgramAt(server.URL), nil), "aura-2-harmonia-en", "")
	if err != nil {
		t.Fatal(err)
	}
	err = a.Stream(context.Background(), "hello", func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || strings.Contains(err.Error(), auraPlanted) || strings.Contains(err.Error(), "Bad Request") {
		t.Fatalf("Stream = %v, want the refusal's status code alone", err)
	}
	var refusal *providererr.Error
	if !errors.As(err, &refusal) || refusal.Provider != "tts" || refusal.Status != http.StatusBadRequest {
		t.Fatalf("Stream = %v (%#v), want a providererr.Error of the provider tts's HTTP 400", err, refusal)
	}
}

// TestAuraStreamWithoutASessionUsesItsOwnSocket fails if Aura's Stream, used
// without a Session, does not dial, speak, and close a socket of its own.
func TestAuraStreamWithoutASessionUsesItsOwnSocket(t *testing.T) {
	pcm := ramp(0, 1400)
	f := startSpeak(t, speakBack(map[string][]byte{"hello": pcm}))
	var got []byte
	if err := newTestAura(t, f).Stream(context.Background(), "hello", func(p []byte) error {
		got = append(got, p...)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("emitted %d bytes, want Deepgram's %d", len(got), len(pcm))
	}
	f.waitEnded(t, 1)
	if want := []string{"Speak hello", "Flush", "Close"}; !slices.Equal(f.received(), want) {
		t.Fatalf("Deepgram got %q, want %q", f.received(), want)
	}
}

// TestAuraTellsDialedTheURLOfEachSocket fails if Dialed is not told, for each
// socket Aura opens (a Session's and a Stream's own), the URL its handshake
// asked Deepgram on: ghost-wispr tts-first-audio reports from it whether the
// request carried mip_opt_out=true.
func TestAuraTellsDialedTheURLOfEachSocket(t *testing.T) {
	f := startSpeak(t, speakBack(map[string][]byte{"hello": ramp(0, 1000)}))
	var mu sync.Mutex
	var dialed []*url.URL
	a := newTestAura(t, f, WithDialed(func(u *url.URL) {
		mu.Lock()
		defer mu.Unlock()
		dialed = append(dialed, u)
	}))
	openSession(t, a, newFakeOutput())
	if err := a.Stream(context.Background(), "hello", func([]byte) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(dialed) != 2 || len(f.queries) != 2 {
		t.Fatalf("Dialed was told %d URLs for %d sockets Deepgram saw, want 2 and 2", len(dialed), len(f.queries))
	}
	for i, u := range dialed {
		if u.Path != "/v1/speak" || !maps.EqualFunc(u.Query(), f.queries[i], slices.Equal) {
			t.Errorf("socket %d: Dialed was told path %q query %v; Deepgram saw path /v1/speak query %v", i+1, u.Path, u.Query(), f.queries[i])
		}
	}
}

// TestAuraWithoutItsKeyDoesNotOpen fails if a Session opens on a Deepgram
// provider whose key is not set, or the failure does not say so.
func TestAuraWithoutItsKeyDoesNotOpen(t *testing.T) {
	f := startSpeak(t, speakBack(nil))
	p := deepgramAt(f.url)
	p.Auth.Env = "EXAMPLE_UNSET_KEY"
	a, err := NewEngine(testProvider(t, p, nil), "aura-2-harmonia-en", "")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if _, err := New(a).Open(context.Background(), &voice.Exchange{Out: newFakeOutput()}); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("Open = %v, want a failure saying the key is not set", err)
	}
	if n := f.connections(); n != 0 {
		t.Fatalf("Deepgram had %d connections, want none without a key", n)
	}
}
