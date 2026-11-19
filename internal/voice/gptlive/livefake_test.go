package gptlive

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// testWait bounds every wait for something the session or the stand-in
// server should do.
const testWait = 3 * time.Second

// liveServer is a stand-in for OpenAI's primary Live WebSocket,
// wss://api.openai.com/v1/live/sessions: it takes connections that carry the
// bearer it expects and hands each to the test, which plays the server's
// side of the documented event sequences.
type liveServer struct {
	t      *testing.T
	srv    *httptest.Server
	bearer string
	conns  chan *liveConn
	// autoAck answers each instructions, thinking and commentary append with
	// its acknowledgement, as the server does once the context is injected.
	autoAck bool
}

func newLiveServer(t *testing.T) *liveServer {
	t.Helper()
	s := &liveServer{t: t, bearer: "access-token", conns: make(chan *liveConn, 4), autoAck: true}
	upgrader := websocket.Upgrader{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/live/sessions" {
			http.Error(w, "no such path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.URL.RawQuery != "" {
			http.Error(w, "Live takes no query parameters", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+s.bearer {
			http.Error(w, `{"error":"invalid_api_key"}`, http.StatusUnauthorized)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := &liveConn{t: t, ws: ws, in: make(chan clientEvent, 4096), done: make(chan struct{}), autoAck: s.autoAck}
		go c.read()
		s.conns <- c
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// url is the server's Live endpoint, as a WebSocket URL.
func (s *liveServer) url() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/v1/live/sessions"
}

// conn waits for the session's connection.
func (s *liveServer) conn() *liveConn {
	s.t.Helper()
	select {
	case c := <-s.conns:
		s.t.Cleanup(func() { _ = c.ws.Close() })
		return c
	case <-time.After(testWait):
		s.t.Fatal("no connection reached the stand-in Live server")
		return nil
	}
}

// clientEvent is one event the session sent, with the fields the tests read.
type clientEvent struct {
	Type         string        `json:"type"`
	EventID      string        `json:"event_id"`
	DelegationID *string       `json:"delegation_id"`
	Content      string        `json:"content"`
	Audio        string        `json:"audio"`
	Session      sessionConfig `json:"session"`
	raw          []byte
}

// liveConn is the server's side of one session's socket.
type liveConn struct {
	t       *testing.T
	ws      *websocket.Conn
	in      chan clientEvent
	done    chan struct{}
	writeMu sync.Mutex
	autoAck bool
}

func (c *liveConn) read() {
	defer close(c.done)
	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			c.t.Errorf("the session sent a non-text message (%d)", kind)
			continue
		}
		var ev clientEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			c.t.Errorf("the session sent unreadable JSON %q: %v", data, err)
			continue
		}
		ev.raw = data
		if c.autoAck {
			switch ev.Type {
			case evInstructionsAppend, evThinkingAppend, evCommentaryAppend:
				c.send(map[string]any{"type": strings.TrimSuffix(ev.Type, "append") + "appended", "event_id": "ack_" + ev.EventID, "client_event_id": ev.EventID, "start_ms": 0, "end_ms": 0})
			}
		}
		c.in <- ev
	}
}

// send writes one server event.
func (c *liveConn) send(v any) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.WriteJSON(v)
}

// next waits for the session's next event that is not an audio append.
func (c *liveConn) next() clientEvent {
	c.t.Helper()
	for {
		select {
		case ev := <-c.in:
			if ev.Type == evInputAudioAppend {
				continue
			}
			return ev
		case <-time.After(testWait):
			c.t.Fatal("the session sent no event")
			return clientEvent{}
		}
	}
}

// expect waits for the session's next event of type typ, passing over others.
func (c *liveConn) expect(typ string) clientEvent {
	c.t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case ev := <-c.in:
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			c.t.Fatalf("the session sent no %s", typ)
			return clientEvent{}
		}
	}
}

// start reads session.start, which must be the session's first event, and
// answers session.started.
func (c *liveConn) start() clientEvent {
	c.t.Helper()
	var ev clientEvent
	select {
	case ev = <-c.in:
	case <-time.After(testWait):
		c.t.Fatal("the session sent no session.start")
	}
	if ev.Type != evSessionStart {
		c.t.Fatalf("first event %q, want %s", ev.Type, evSessionStart)
	}
	c.send(map[string]any{"type": evSessionStarted, "event_id": "event_started", "session": map[string]any{"id": "sess_test", "model": "gpt-live-1"}})
	return ev
}

// audioDelta sends model speech: samples of PCM16 at the session's rate.
func (c *liveConn) audioDelta(samples []int16) {
	c.send(map[string]any{"type": evOutputAudioDelta, "delta": base64.StdEncoding.EncodeToString(pcmBytes(samples))})
}

func (c *liveConn) delegate(id string) {
	c.send(map[string]any{"type": evDelegationCreated, "event_id": "event_" + id, "offset_ms": 1000, "delegation": map[string]any{"id": id, "type": "delegation", "target": "client"}})
}

func (c *liveConn) heard(text string) {
	c.send(map[string]any{"type": evInputTranscriptDelta, "event_id": "event_in", "delta": text, "start_ms": 0, "end_ms": 100})
}

func (c *liveConn) said(text string) {
	c.send(map[string]any{"type": evOutputTranscript, "event_id": "event_out", "delta": text, "start_ms": 0, "end_ms": 100})
}

// closed waits for the session to close its socket.
func (c *liveConn) closed() {
	c.t.Helper()
	select {
	case <-c.done:
	case <-time.After(testWait):
		c.t.Fatal("the session did not close its socket")
	}
}

func pcmBytes(samples []int16) []byte {
	b := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	return b
}

func decodeAudio(t *testing.T, b64 string) []int16 {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("audio is not base64: %v", err)
	}
	if len(b)%2 != 0 {
		t.Fatalf("audio of %d bytes is not whole PCM16 samples", len(b))
	}
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s
}

// ramp is n samples counting up from start, so a test can tell which
// samples played.
func ramp(start, n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(start + i)
	}
	return s
}

var errTestCancelled = errors.New("playback cancelled")

// fakeOutput is the speaker output's voice input: it records each speech's
// frames and lets a test cancel a speech, as the Supervisor's Out.Cancel does.
type fakeOutput struct {
	mu     sync.Mutex
	speech []*fakePlayback
	begun  chan *fakePlayback
}

func newFakeOutput() *fakeOutput { return &fakeOutput{begun: make(chan *fakePlayback, 16)} }

func (o *fakeOutput) Begin() voice.Playback {
	p := &fakePlayback{wrote: make(chan struct{}, 1)}
	o.mu.Lock()
	o.speech = append(o.speech, p)
	o.mu.Unlock()
	o.begun <- p
	return p
}

// speeches returns every speech begun so far.
func (o *fakeOutput) speeches() []*fakePlayback {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*fakePlayback(nil), o.speech...)
}

// samples returns every sample written on every speech, in order.
func (o *fakeOutput) samples() []int16 {
	var all []int16
	for _, p := range o.speeches() {
		all = append(all, p.samples()...)
	}
	return all
}

// nextSpeech waits for the session to begin a speech.
func (o *fakeOutput) nextSpeech(t *testing.T) *fakePlayback {
	t.Helper()
	select {
	case p := <-o.begun:
		return p
	case <-time.After(testWait):
		t.Fatal("the session began no speech on the voice input")
		return nil
	}
}

type fakePlayback struct {
	mu        sync.Mutex
	frames    [][]byte
	formats   []audio.AudioFormat
	cancelled bool
	drained   bool
	wrote     chan struct{}
}

func (p *fakePlayback) Write(_ context.Context, frame []byte, format audio.AudioFormat) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancelled {
		return errTestCancelled
	}
	p.frames = append(p.frames, append([]byte(nil), frame...))
	p.formats = append(p.formats, format)
	select {
	case p.wrote <- struct{}{}:
	default:
	}
	return nil
}

func (p *fakePlayback) Drain(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drained = true
	if p.cancelled {
		return errTestCancelled
	}
	return nil
}

func (p *fakePlayback) cancel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelled = true
}

func (p *fakePlayback) samples() []int16 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var s []int16
	for _, f := range p.frames {
		for i := 0; i+1 < len(f); i += 2 {
			s = append(s, int16(binary.LittleEndian.Uint16(f[i:])))
		}
	}
	return s
}

// waitSamples waits until the speech holds n samples.
func (p *fakePlayback) waitSamples(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(testWait)
	for len(p.samples()) < n {
		select {
		case <-p.wrote:
		case <-deadline:
			t.Fatalf("the speech holds %d samples, want %d", len(p.samples()), n)
		}
	}
}

// fakeTools is the Concierge's tool runner.
type fakeTools struct {
	calls  chan voice.ToolCall
	result json.RawMessage
	err    error
	block  bool // Run waits for its context to end
}

func newFakeTools(result voice.AnswerResult) *fakeTools {
	b, _ := json.Marshal(result)
	return &fakeTools{calls: make(chan voice.ToolCall, 8), result: b}
}

func (f *fakeTools) Run(ctx context.Context, c voice.ToolCall) (json.RawMessage, error) {
	f.calls <- c
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.result, f.err
}

func (f *fakeTools) call(t *testing.T) voice.ToolCall {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-time.After(testWait):
		t.Fatal("the session ran no tool")
		return voice.ToolCall{}
	}
}

// testExchange is an exchange with a fake output and tools and no tap.
func testExchange(out *fakeOutput, tools voice.ToolRunner) voice.Exchange {
	return voice.Exchange{
		SessionID: "session-1",
		WakeAt:    time.Unix(1_800_000_000, 0),
		AskerTurn: "what is happening with ABC-123",
		Recent: []voice.TurnText{
			{Speaker: 0, Text: "Let's check on the recorder work.", At: time.Unix(1_799_999_990, 0)},
			{Speaker: 1, Text: "Wispr knows.", At: time.Unix(1_799_999_995, 0)},
		},
		Out:   out,
		Tools: tools,
	}
}

// openSession opens a session on srv for x, playing the server's side of
// the start, and returns it with the server's side of its socket.
func openSession(t *testing.T, srv *liveServer, x voice.Exchange) (voice.Session, *liveConn, clientEvent) {
	t.Helper()
	v, err := New(Config{URL: srv.url(), Token: func(context.Context) (string, error) { return "access-token", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type opened struct {
		s   voice.Session
		err error
	}
	done := make(chan opened, 1)
	go func() {
		s, err := v.Open(context.Background(), x)
		done <- opened{s, err}
	}()
	c := srv.conn()
	start := c.start()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("Open: %v", o.err)
		}
		t.Cleanup(func() { _ = o.s.Close() })
		return o.s, c, start
	case <-time.After(testWait):
		t.Fatal("Open did not return after session.started")
		return nil, nil, clientEvent{}
	}
}

// waitEvent waits for the session's next event of kind, passing over others.
func waitEvent(t *testing.T, s voice.Session, kind voice.EventKind) voice.Event {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatalf("Events closed before an event of kind %d", kind)
			}
			if e.Kind == kind {
				return e
			}
		case <-deadline:
			t.Fatalf("no event of kind %d", kind)
			return voice.Event{}
		}
	}
}
