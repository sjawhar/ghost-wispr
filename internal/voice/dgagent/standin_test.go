package dgagent

// A stand-in for Deepgram's Voice Agent server, and fakes for the Output and
// the Concierge, so the tests replay the documented message flows
// (developers.deepgram.com/docs/voice-agent-message-flow and the pages it
// links) without a network.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// standIn is a Voice Agent endpoint. Each connection sends Welcome, as
// Deepgram does, and is handed to the test.
type standIn struct {
	url      string
	conns    chan *agentConn
	overflow atomic.Bool // a connection sent more frames than the test read
}

// clientFrame is one message the session sent.
type clientFrame struct {
	binary bool
	typ    string // the "type" of a text frame
	raw    []byte // the JSON of a text frame, or the PCM of a binary one
}

// agentConn is the server side of one session's connection.
type agentConn struct {
	header http.Header
	ws     *websocket.Conn
	frames chan clientFrame
	ended  chan struct{} // closed when the client's side has gone
	mu     sync.Mutex
}

func startStandIn(t *testing.T) *standIn {
	t.Helper()
	s := &standIn{conns: make(chan *agentConn, 4)}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := &agentConn{
			header: r.Header.Clone(),
			ws:     ws,
			frames: make(chan clientFrame, 4096),
			ended:  make(chan struct{}),
		}
		defer close(c.ended)
		defer func() { _ = ws.Close() }()
		c.send(map[string]any{"type": "Welcome", "request_id": "standin-request"})
		s.conns <- c
		for {
			kind, message, err := ws.ReadMessage()
			if err != nil {
				return
			}
			f := clientFrame{binary: kind == websocket.BinaryMessage, raw: message}
			if !f.binary {
				var head struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(message, &head); err != nil {
					head.Type = "unparsable"
				}
				f.typ = head.Type
			}
			select {
			case c.frames <- f:
			default:
				s.overflow.Store(true) // reported by the test, not this goroutine
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		if s.overflow.Load() {
			t.Errorf("stand-in: a session sent more than 4096 frames unread")
		}
	})
	s.url = "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/agent/converse"
	return s
}

// accept waits for the session's connection.
func (s *standIn) accept(t *testing.T) *agentConn {
	t.Helper()
	select {
	case c := <-s.conns:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("the session never connected to the stand-in")
		return nil
	}
}

// send writes one JSON message to the session.
func (c *agentConn) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteMessage(websocket.TextMessage, data)
}

// sendAudio writes one binary frame of agent speech.
func (c *agentConn) sendAudio(pcm []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteMessage(websocket.BinaryMessage, pcm)
}

// drop closes the connection the way a failing network or server would.
func (c *agentConn) drop() {
	_ = c.ws.Close()
}

// dropWith closes the connection with a close frame of code and text, as
// Deepgram does when it ends a session.
func (c *agentConn) dropWith(code int, text string) {
	c.mu.Lock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(time.Second))
	c.mu.Unlock()
	_ = c.ws.Close()
}

// next returns the next frame the session sent that is not a KeepAlive.
func (c *agentConn) next(t *testing.T, within time.Duration) (clientFrame, bool) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f := <-c.frames:
			if f.typ == "KeepAlive" {
				continue
			}
			return f, true
		case <-deadline:
			return clientFrame{}, false
		}
	}
}

// nextText returns the next text frame, skipping audio and KeepAlive.
func (c *agentConn) nextText(t *testing.T, within time.Duration) clientFrame {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		f, ok := c.next(t, time.Until(deadline))
		if !ok {
			t.Fatalf("the session sent no text message within %v", within)
		}
		if !f.binary {
			return f
		}
	}
}

// nextAudio returns the PCM of the next binary frame, skipping text.
func (c *agentConn) nextAudio(t *testing.T) []byte {
	t.Helper()
	for {
		f, ok := c.next(t, 3*time.Second)
		if !ok {
			t.Fatal("no audio reached Deepgram")
		}
		if f.binary {
			return f.raw
		}
	}
}

// expect returns the next text frame and fails unless it has type typ.
func (c *agentConn) expect(t *testing.T, typ string) map[string]any {
	t.Helper()
	f := c.nextText(t, 3*time.Second)
	if f.typ != typ {
		t.Fatalf("the session sent %s (%s), want %s", f.typ, f.raw, typ)
	}
	var m map[string]any
	if err := json.Unmarshal(f.raw, &m); err != nil {
		t.Fatalf("decode %s: %v", f.raw, err)
	}
	return m
}

// quiet fails if the session sends a text message of type typ within d.
func (c *agentConn) quiet(t *testing.T, d time.Duration, typ string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		f, ok := c.next(t, time.Until(deadline))
		if !ok {
			return
		}
		if !f.binary && f.typ == typ {
			t.Fatalf("the session sent %s, want none: %s", typ, f.raw)
		}
	}
}

// field reads a nested field of a decoded JSON object.
func field(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = obj[p]
	}
	return v
}

// tools is a Concierge that answers each call with fn.
type tools func(ctx context.Context, c voice.ToolCall) (json.RawMessage, error)

func (f tools) Run(ctx context.Context, c voice.ToolCall) (json.RawMessage, error) { return f(ctx, c) }

// fakeOutput is the speaker's "voice" input. Each Begin makes a playback the
// test can read; writes block while blockWrites is set, until their context
// ends, as a full Output queue would.
type fakeOutput struct {
	mu          sync.Mutex
	plays       []*fakePlayback
	begun       chan *fakePlayback
	blockWrites bool
}

func newFakeOutput() *fakeOutput { return &fakeOutput{begun: make(chan *fakePlayback, 16)} }

func (o *fakeOutput) Begin() voice.Playback {
	o.mu.Lock()
	p := &fakePlayback{block: o.blockWrites, wrote: make(chan struct{}, 4096)}
	o.plays = append(o.plays, p)
	o.mu.Unlock()
	o.begun <- p
	return p
}

func (o *fakeOutput) playbacks() []*fakePlayback {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*fakePlayback(nil), o.plays...)
}

// nextPlayback waits for the next Begin.
func (o *fakeOutput) nextPlayback(t *testing.T) *fakePlayback {
	t.Helper()
	select {
	case p := <-o.begun:
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("the session began no playback")
		return nil
	}
}

type fakePlayback struct {
	mu      sync.Mutex
	pcm     []byte
	formats []audio.AudioFormat
	longest int // the longest frame written, in bytes
	block   bool
	drained bool
	wrote   chan struct{}
}

func (p *fakePlayback) Write(ctx context.Context, frame []byte, format audio.AudioFormat) error {
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.pcm = append(p.pcm, frame...)
	p.formats = append(p.formats, format)
	p.longest = max(p.longest, len(frame))
	p.mu.Unlock()
	p.wrote <- struct{}{}
	return nil
}

func (p *fakePlayback) Drain(context.Context) error {
	p.mu.Lock()
	p.drained = true
	p.mu.Unlock()
	return nil
}

func (p *fakePlayback) written() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.pcm...)
}

// waitBytes waits until the playback holds n bytes.
func (p *fakePlayback) waitBytes(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for len(p.written()) < n {
		select {
		case <-p.wrote:
		case <-deadline:
			t.Fatalf("the playback got %d bytes, want %d", len(p.written()), n)
		}
	}
}

// pcm16 is samples as PCM16 little-endian bytes.
func pcm16(samples ...int16) []byte {
	b := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	return b
}

// tone is d of a constant sample at rate, as PCM16 bytes.
func tone(rate int, d time.Duration, sample int16) []byte {
	n := int(int64(rate) * int64(d) / int64(time.Second))
	s := make([]int16, n)
	for i := range s {
		s[i] = sample
	}
	return pcm16(s...)
}

// waitEvent returns the next event of kind, failing on any Err event unless
// kind is EventErr.
func waitEvent(t *testing.T, s voice.Session, kind voice.EventKind) voice.Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatalf("the events closed before a %v", kind)
			}
			if e.Kind == kind {
				return e
			}
			if e.Kind == voice.EventErr {
				t.Fatalf("event Err %v while waiting for %v", e.Err, kind)
			}
		case <-deadline:
			t.Fatalf("no event %v within 3s", kind)
		}
	}
}

// waitEventsClosed fails unless the events close within a second, and
// returns those it read on the way.
func waitEventsClosed(t *testing.T, s voice.Session) []voice.Event {
	t.Helper()
	var got []voice.Event
	deadline := time.After(time.Second)
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				return got
			}
			got = append(got, e)
		case <-deadline:
			t.Fatal("the events did not close after Close")
		}
	}
}

// open starts a session against the stand-in, plays the handshake, and
// returns the session, its connection and the Settings it sent.
//
//nolint:gocritic // Each test passes its own Config literal.
func open(t *testing.T, s *standIn, cfg Config, x voice.Exchange) (voice.Session, *agentConn, map[string]any) {
	t.Helper()
	cfg.URL = s.url
	if cfg.APIKey == "" {
		cfg.APIKey = standInKey
	}
	if cfg.speechGap == 0 {
		cfg.speechGap = testSpeechGap
	}
	agent, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type opened struct {
		s   voice.Session
		err error
	}
	done := make(chan opened, 1)
	go func() {
		sess, err := agent.Open(context.Background(), &x)
		done <- opened{sess, err}
	}()
	c := s.accept(t)
	settings := c.expect(t, "Settings")
	c.send(map[string]any{"type": "SettingsApplied"})
	var o opened
	select {
	case o = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Open did not return after SettingsApplied")
	}
	if o.err != nil {
		t.Fatalf("Open: %v", o.err)
	}
	t.Cleanup(func() { _ = o.s.Close() })
	return o.s, c, settings
}

// injectSpeech starts a session-owned injection and gives the stand-in its
// text and audio. Tests that exercise playback use it: unmatched vendor
// audio is never authorized to reach Output.
func injectSpeech(t *testing.T, sess voice.Session, c *agentConn, text string, pcm []byte, ended bool) chan error {
	t.Helper()
	done := sayAsync(sess, text)
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": text})
	c.sendAudio(pcm)
	if ended {
		c.send(map[string]any{"type": "AgentAudioDone"})
	}
	return done
}

// testSpeechGap, when set, is the speech gap of the sessions open() makes.
var testSpeechGap time.Duration

// setSpeechGap sets how long the player waits for more of a reply before it
// takes the reply as ended, for the sessions one test opens.
func setSpeechGap(t *testing.T, d time.Duration) {
	t.Helper()
	testSpeechGap = d
	t.Cleanup(func() { testSpeechGap = 0 })
}

// standInKey is the key sessions present to the stand-in, which takes any.
const standInKey = "stand-in-key"

// stallConn is a client's TCP connection that, once stalled, takes no more
// data, as on a network whose buffers are full: a write waits for its
// deadline or for the connection to close.
type stallConn struct {
	net.Conn
	mu       sync.Mutex
	stalled  bool
	deadline time.Time // for writes
	closed   chan struct{}
	once     sync.Once
}

func (c *stallConn) stall() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stalled = true
}

func (c *stallConn) SetDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetDeadline(t)
}

func (c *stallConn) SetWriteDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *stallConn) setWriteDeadline(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
}

func (c *stallConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	stalled, deadline := c.stalled, c.deadline
	c.mu.Unlock()
	if !stalled {
		return c.Conn.Write(p)
	}
	var expired <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-expired:
		return 0, os.ErrDeadlineExceeded
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *stallConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// startTLS starts a session on a wss connection, as Deepgram's is, to a
// stand-in that reads all it is sent, and returns the session with the
// client's TCP connection under the TLS. The stand-in plays no handshake:
// the session starts as Open starts it once Deepgram applied the Settings.
func startTLS(t *testing.T) (voice.Session, *stallConn) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	var raw *stallConn
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			raw = &stallConn{Conn: conn, closed: make(chan struct{})}
			return raw, nil
		},
	}
	conn, resp, err := dialer.DialContext(context.Background(), "wss"+strings.TrimPrefix(srv.URL, "https"), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial the TLS stand-in: %v", err)
	}
	agent, err := New(&Config{APIKey: standInKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := start(&agent.cfg, conn, &voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	t.Cleanup(func() { _ = sess.Close() })
	return sess, raw
}
