package tts

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/netdial"
	"github.com/sjawhar/ghost-wispr/internal/spokentext"
)

const (
	auraDefaultBaseURL = "wss://api.deepgram.com"
	// auraDefaultModel is Aura-2's Harmonia, whom Deepgram's voice list calls
	// empathetic, clear, calm and confident, for customer service: a calm
	// voice for answers in a meeting.
	auraDefaultModel = "aura-2-harmonia-en"
	// auraRate is the rate voice A asks Aura-2 for, the rate every voice
	// speaks into the Output's mixer.
	auraRate = 24000
	// auraWriteTimeout bounds one write to the socket, and is the socket's
	// TCP_USER_TIMEOUT. A write that takes longer closes the socket.
	auraWriteTimeout = 5 * time.Second
	auraDialTimeout  = 15 * time.Second
	// auraMessages is how many of Deepgram's messages the reader holds for a
	// Stream that is waiting on the Output.
	auraMessages = 32
)

// auraStallTimeout is the longest a Stream waits for Deepgram's next message
// after a Flush, or for its Cleared after a Clear, before it gives the
// socket up. A var so tests can shorten it.
var auraStallTimeout = 5 * time.Second

var errAuraSocketClosed = errors.New("aura: socket closed")

var (
	auraFlush = []byte(`{"type":"Flush"}`)
	auraClear = []byte(`{"type":"Clear"}`)
	auraClose = []byte(`{"type":"Close"}`)
)

// AuraConfig configures the Deepgram Aura-2 engine.
type AuraConfig struct {
	APIKey string
	// Model is an Aura-2 voice; "aura-2-harmonia-en" when empty.
	Model string
	// BaseURL replaces "wss://api.deepgram.com", for tests.
	BaseURL string
	// Dialed, when set, is told the URL of each socket's handshake request,
	// as it went to Deepgram, once Deepgram accepted it: ghost-wispr
	// tts-first-audio reads from it whether the request carried
	// mip_opt_out=true.
	Dialed func(*url.URL)
}

// Aura synthesises speech with Deepgram Aura-2 over its streaming WebSocket,
// /v1/speak, which takes Speak and Flush messages and sends headerless PCM16
// back as binary messages, then Flushed. Every socket asks Deepgram to keep
// the text out of its model training (mip_opt_out=true).
//
// Aura is a Connector: a Session holds one socket from Open to Close, so its
// first Say does not wait for the dial, and an Interrupt sends Clear, which
// drops the rest of the text in flight.
type Aura struct {
	apiKey string
	url    string
	dialed func(*url.URL)
}

// NewAura returns an Aura-2 engine for cfg.
func NewAura(cfg AuraConfig) (*Aura, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, errors.New("aura: a Deepgram API key is required")
	}
	query := url.Values{
		"model":       {cmp.Or(strings.TrimSpace(cfg.Model), auraDefaultModel)},
		"encoding":    {"linear16"},
		"sample_rate": {strconv.Itoa(auraRate)},
		"mip_opt_out": {"true"},
	}
	base := strings.TrimRight(cmp.Or(strings.TrimSpace(cfg.BaseURL), auraDefaultBaseURL), "/")
	return &Aura{apiKey: key, url: base + "/v1/speak?" + query.Encode(), dialed: cfg.Dialed}, nil
}

// Name names the engine.
func (*Aura) Name() string { return "aura" }

// Format is PCM16 mono at 24 kHz, as every socket asks for.
func (*Aura) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: auraRate, Channels: 1, Encoding: audio.EncodingPCM16}
}

// Connect opens the socket a Session speaks over.
func (a *Aura) Connect(ctx context.Context) (Conn, error) {
	s, err := a.dial(ctx)
	if err != nil {
		return nil, err
	}
	return &auraConn{aura: a, socket: s}, nil
}

// Stream speaks text over a socket of its own, for use without a Session.
func (a *Aura) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	s, err := a.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	return s.stream(ctx, text, emit)
}

// auraConn is a Session's connection to Aura-2: one socket, dialled again
// when Deepgram has closed it.
type auraConn struct {
	aura   *Aura
	mu     sync.Mutex
	socket *auraSocket
	closed bool
}

// Stream speaks text on the Session's socket. When the socket fails before
// any of the text's audio arrived, as one Deepgram closed while idle does, it
// says the text again on a new socket, once.
func (c *auraConn) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	s, err := c.ready(ctx)
	if err != nil {
		return err
	}
	emitted := false
	err = s.stream(ctx, text, func(pcm []byte) error {
		emitted = true
		return emit(pcm)
	})
	if err == nil || emitted || ctx.Err() != nil || !s.ended() {
		return err
	}
	if s, err = c.redial(ctx); err != nil {
		return err
	}
	return s.stream(ctx, text, emit)
}

// ready returns the socket to speak on: the open one once Deepgram has
// confirmed its last Clear, else a new one.
func (c *auraConn) ready(ctx context.Context) (*auraSocket, error) {
	c.mu.Lock()
	s, closed := c.socket, c.closed
	c.mu.Unlock()
	if closed {
		return nil, errAuraSocketClosed
	}
	if s.clearing {
		if err := s.awaitCleared(ctx); err != nil {
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			s.abort()
		}
	}
	if !s.ended() {
		return s, nil
	}
	return c.redial(ctx)
}

func (c *auraConn) redial(ctx context.Context) (*auraSocket, error) {
	s, err := c.aura.dial(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	old, closed := c.socket, c.closed
	if !closed {
		c.socket = s
	}
	c.mu.Unlock()
	if closed {
		_ = s.Close()
		return nil, errAuraSocketClosed
	}
	_ = old.Close()
	return s, nil
}

// Close closes the socket, which ends a Stream waiting on it.
func (c *auraConn) Close() error {
	c.mu.Lock()
	s := c.socket
	c.closed = true
	c.mu.Unlock()
	return s.Close()
}

// auraSocket is one /v1/speak WebSocket. It owns the TCP connection, as
// cmd/ghost-wispr's live-transcription socket does: every write has a
// deadline, the connection has TCP_USER_TIMEOUT so the kernel gives up on
// unacknowledged data in the same time, and a failed write resets and closes
// the connection, which ends the reader. One reader goroutine hands
// Deepgram's messages on in order.
type auraSocket struct {
	conn    *websocket.Conn
	raw     net.Conn
	writeMu sync.Mutex
	closed  atomic.Bool
	stop    chan struct{} // closed by abort: the reader hands nothing more on
	msgs    chan auraMessage
	done    chan struct{} // closed when the reader returns
	err     error         // why the reader returned, when not closed by us; read after done

	// clearing is set when a Clear awaits its Cleared. Only the goroutine
	// that streams touches it.
	clearing bool
}

// auraMessage is one of Deepgram's messages: audio, or a text message's type.
type auraMessage struct {
	binary  bool
	audio   []byte
	kind    string // Flushed, Cleared, Metadata or Warning
	warning string
}

type auraSpeak struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (a *Aura) dial(ctx context.Context) (*auraSocket, error) {
	var raw net.Conn
	tcp := &net.Dialer{Timeout: auraDialTimeout, Control: netdial.TCPUserTimeout(auraWriteTimeout)}
	dialer := websocket.Dialer{
		HandshakeTimeout: auraDialTimeout,
		NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := tcp.DialContext(ctx, network, address)
			raw = conn
			return conn, err
		},
	}
	conn, response, err := dialer.DialContext(ctx, a.url, http.Header{"Authorization": {"Token " + a.apiKey}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("aura: connect: %w (%s %s)", err, response.Status, response.Header.Get("dg-error"))
		}
		return nil, fmt.Errorf("aura: connect: %w", err)
	}
	if a.dialed != nil && response != nil && response.Request != nil {
		a.dialed(response.Request.URL)
	}
	s := &auraSocket{
		conn: conn,
		raw:  raw,
		stop: make(chan struct{}),
		msgs: make(chan auraMessage, auraMessages),
		done: make(chan struct{}),
	}
	go s.read()
	return s, nil
}

// stream sends text and a Flush, and emits the audio Deepgram sends back
// until its Flushed. When ctx ends or emit fails, it sends Clear and returns
// without waiting for the Cleared, which the next stream waits for.
func (s *auraSocket) stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	speak, err := json.Marshal(auraSpeak{Type: "Speak", Text: text})
	if err != nil {
		return fmt.Errorf("aura: %w", err)
	}
	if err := s.send(speak); err != nil {
		return err
	}
	if err := s.send(auraFlush); err != nil {
		return err
	}
	stall := time.NewTimer(auraStallTimeout)
	defer stall.Stop()
	var warning string
	handle := func(m auraMessage) (flushed bool, err error) {
		switch {
		case m.binary:
			if err := emit(m.audio); err != nil {
				s.clear()
				return false, err
			}
		case m.kind == "Flushed":
			return true, nil
		case m.kind == "Warning":
			warning = m.warning
		}
		stall.Reset(auraStallTimeout)
		return false, nil
	}
	for {
		var m auraMessage
		select {
		case m = <-s.msgs:
		case <-ctx.Done():
			s.clear()
			return context.Cause(ctx)
		case <-s.done:
			// What the reader handed on before it ended comes first.
			select {
			case m = <-s.msgs:
			default:
				return s.endErr()
			}
		case <-stall.C:
			s.abort()
			if warning != "" {
				return fmt.Errorf("aura: no audio for %s after Deepgram warned: %s", auraStallTimeout, spokentext.Withhold(warning, text))
			}
			return fmt.Errorf("aura: no audio for %s after a Flush", auraStallTimeout)
		}
		if flushed, err := handle(m); flushed || err != nil {
			return err
		}
	}
}

// clear asks Deepgram to drop the rest of the text in flight. A socket whose
// Clear fails is closed, and the next stream dials another.
func (s *auraSocket) clear() {
	if s.send(auraClear) == nil {
		s.clearing = true
	}
}

// awaitCleared drops the audio of a cleared text until Deepgram's Cleared.
func (s *auraSocket) awaitCleared(ctx context.Context) error {
	stall := time.NewTimer(auraStallTimeout)
	defer stall.Stop()
	for {
		select {
		case m := <-s.msgs:
			if m.kind == "Cleared" {
				s.clearing = false
				return nil
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-s.done:
			return s.endErr()
		case <-stall.C:
			return fmt.Errorf("aura: no Cleared %s after a Clear", auraStallTimeout)
		}
	}
}

func (s *auraSocket) send(msg []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return errAuraSocketClosed
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(auraWriteTimeout)); err != nil {
		s.abort()
		return fmt.Errorf("aura: write: %w", err)
	}
	if err := s.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		s.abort()
		return fmt.Errorf("aura: write: %w", err)
	}
	return nil
}

// ended reports whether the socket can no longer speak.
func (s *auraSocket) ended() bool { return s.closed.Load() }

// endErr is why the reader ended. Call it only after done.
func (s *auraSocket) endErr() error {
	if s.err != nil {
		return fmt.Errorf("aura: %w", s.err)
	}
	return errAuraSocketClosed
}

// Close tells Deepgram the socket ends, unless a write is in progress, which
// can be one stuck on a stalled peer, and closes the connection.
func (s *auraSocket) Close() error {
	if s.writeMu.TryLock() {
		if !s.closed.Load() {
			_ = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = s.conn.WriteMessage(websocket.TextMessage, auraClose)
		}
		s.writeMu.Unlock()
	}
	s.abort()
	<-s.done
	return nil
}

// abort closes the connection at once: a reset, so nothing unsent keeps the
// socket alive in the kernel. A write blocked on it returns, and so does the
// reader.
func (s *auraSocket) abort() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.stop)
	if tcp, ok := s.raw.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = s.conn.Close()
}

// read hands Deepgram's messages on until the connection ends.
func (s *auraSocket) read() {
	defer close(s.done)
	for {
		kind, data, err := s.conn.ReadMessage()
		if err != nil {
			if !s.closed.Load() && !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				s.err = err
			}
			s.abort()
			return
		}
		m := auraMessage{binary: true, audio: data}
		if kind != websocket.BinaryMessage {
			var head struct {
				Type        string `json:"type"`
				Description string `json:"description"`
				Code        string `json:"code"`
			}
			if json.Unmarshal(data, &head) != nil {
				continue
			}
			m = auraMessage{kind: head.Type}
			if head.Type == "Warning" {
				m.warning = fmt.Sprintf("%s (%s)", head.Description, head.Code)
			}
		}
		select {
		case s.msgs <- m:
		case <-s.stop:
			return
		}
	}
}
