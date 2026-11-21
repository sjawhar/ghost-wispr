package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	websocketv1api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket"
	msginterfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	version "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/version"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/netdial"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// deepgramWriteTimeout bounds one write to a Deepgram socket, and is the
// socket's TCP_USER_TIMEOUT. A write that takes longer fails and closes the
// socket, so the resilient client buffers and dials a new one.
const deepgramWriteTimeout = 5 * time.Second

// deepgramKeepAliveEvery is how often an idle socket tells Deepgram it is
// still in use, as the SDK's keepalive does.
const deepgramKeepAliveEvery = 5 * time.Second

// errDeepgramSocketClosed is a write to a socket that has been closed.
var errDeepgramSocketClosed = errors.New("deepgram socket closed")

// deepgramOptOut keeps a socket's audio and transcripts out of Deepgram's
// Model Improvement Program, which may otherwise keep them after processing,
// for training. The SDK's LiveTranscriptionOptions has no field for
// mip_opt_out, so it goes in as the SDK's custom query parameters; the SDK
// only reads the map, so every dial shares this one.
var deepgramOptOut = map[string][]string{"mip_opt_out": {"true"}}

// deepgramSocket is one live-transcription websocket to Deepgram that the app
// dials itself, so it owns the TCP connection: the SDK's client holds its
// connection's lock through a write that has no deadline, and a peer or path
// that stops taking data then blocks it, and every later call, for good,
// leaking the socket and its goroutines on each reconnect. Every write here
// has a deadline, the connection has TCP_USER_TIMEOUT so the kernel gives up
// on unacknowledged data in the same time, and a failed write resets and
// closes the connection, which ends the socket's reader and keepalive.
// Messages reach the callback through the SDK's own router.
type deepgramSocket struct {
	conn    *websocket.Conn
	raw     net.Conn
	router  *websocketv1api.CallbackRouter
	timeout time.Duration
	writeMu sync.Mutex
	closed  atomic.Bool
	done    chan struct{}

	// A Configure waits for Deepgram's answer before the next is sent
	// (Deepgram refuses a second, CONFIGURE_ALREADY_PENDING): configWant is
	// the newest keyterms not sent yet, when configWaiting; configSent is the
	// keyterms of the one that waits for its answer, and configAnswer hears
	// the answer (OnConfigured).
	configMu      sync.Mutex
	configPending bool
	configWaiting bool
	configWant    []string
	configSent    []string
	configAnswer  func(keyterms []string, taken bool)
}

// dialDeepgramLive opens a live-transcription socket to Deepgram with
// cOptions' host and credentials and tOptions' query, as the SDK's client
// would, and with mip_opt_out=true (deepgramOptOut). deepgramOptOut takes the
// place of any custom query parameters ctx carries, so none can contradict it.
func dialDeepgramLive(ctx context.Context, apiKey string, cOptions *interfaces.ClientOptions, tOptions *interfaces.LiveTranscriptionOptions, callback msginterfaces.LiveMessageCallback, timeout time.Duration) (*deepgramSocket, error) {
	if apiKey != "" {
		cOptions.APIKey = apiKey
	}
	if err := cOptions.Parse(); err != nil {
		return nil, fmt.Errorf("deepgram client options: %w", err)
	}
	if err := tOptions.Check(); err != nil {
		return nil, fmt.Errorf("deepgram live options: %w", err)
	}
	url, err := version.GetLiveAPI(interfaces.WithCustomParameters(ctx, deepgramOptOut), cOptions.Host, cOptions.APIVersion, cOptions.Path, tOptions)
	if err != nil {
		return nil, fmt.Errorf("deepgram live URL: %w", err)
	}
	header := http.Header{}
	if token, bearer := cOptions.GetAuthToken(); bearer {
		header.Set("Authorization", "Bearer "+token)
	} else {
		header.Set("Authorization", "token "+token)
	}
	header.Set("User-Agent", interfaces.DgAgent)
	var raw net.Conn
	tcp := &net.Dialer{Timeout: 15 * time.Second, Control: netdial.TCPUserTimeout(timeout)}
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := tcp.DialContext(ctx, network, address)
			raw = conn
			return conn, err
		},
	}
	conn, response, err := dialer.DialContext(ctx, url, header)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		// A refused handshake keeps Deepgram's status; any other failure,
		// such as an answer net/http could not read, can quote the answer.
		if response != nil {
			return nil, fmt.Errorf("deepgram connect: %w (HTTP %d)", err, response.StatusCode)
		}
		return nil, fmt.Errorf("deepgram connect: %w", providererr.From("deepgram", err))
	}
	s := &deepgramSocket{
		conn:    conn,
		raw:     raw,
		router:  websocketv1api.NewCallbackRouter(callback),
		timeout: timeout,
		done:    make(chan struct{}),
	}
	_ = s.router.Open(&msginterfaces.OpenResponse{Type: string(msginterfaces.TypeOpenResponse)})
	go s.read()
	if cOptions.EnableKeepAlive {
		go s.keepAlive()
	}
	return s, nil
}

// Write sends one buffer of audio.
func (s *deepgramSocket) Write(p []byte) (int, error) {
	if err := s.write(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Finalize asks Deepgram to return final results for the audio it has.
func (s *deepgramSocket) Finalize() error {
	return s.write(websocket.TextMessage, []byte(`{"type":"Finalize"}`))
}

// Configure replaces the stream's keyterms mid-stream, with Deepgram's
// Configure message (transcript.Configurer). It returns at once: the message
// goes when no other waits for Deepgram's answer, and a list given meanwhile
// replaces the one still to send.
func (s *deepgramSocket) Configure(keyterms []string) error {
	if s.closed.Load() {
		return errDeepgramSocketClosed
	}
	s.configMu.Lock()
	s.configWant, s.configWaiting = slices.Clone(keyterms), true
	send := !s.configPending
	s.configPending = true
	s.configMu.Unlock()
	if send {
		go s.sendConfigure()
	}
	return nil
}

// OnConfigured makes answer hear Deepgram's answer to each Configure, with
// the keyterms it answers (transcript.Configurer). A socket that closes
// first never answers: the next dial carries the keyterms.
func (s *deepgramSocket) OnConfigured(answer func(keyterms []string, taken bool)) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.configAnswer = answer
}

// sendConfigure sends the newest keyterms waiting, if any; configPending is
// set.
func (s *deepgramSocket) sendConfigure() {
	s.configMu.Lock()
	if !s.configWaiting {
		s.configPending = false
		s.configMu.Unlock()
		return
	}
	want := s.configWant
	s.configWant, s.configWaiting, s.configSent = nil, false, want
	s.configMu.Unlock()
	body, err := json.Marshal(struct {
		Type     string   `json:"type"`
		Keyterms []string `json:"keyterms"`
	}{"Configure", want})
	if err == nil {
		err = s.write(websocket.TextMessage, body)
	}
	if err != nil {
		// The socket is closing; the next dial carries the keyterms.
		s.configMu.Lock()
		s.configPending = false
		s.configMu.Unlock()
	}
}

// configured takes Deepgram's answer to a Configure: a refusal is logged by
// its code, the answer goes to OnConfigured's hook with the keyterms it
// answers, and the keyterms that waited meanwhile go next.
func (s *deepgramSocket) configured(message []byte) {
	var answer struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(message, &answer)
	taken := answer.Type == "ConfigureSuccess"
	if !taken {
		slog.Warn("deepgram refused the keyterms of a join or a leave", "provider", "deepgram", "code", providererr.Token(answer.Code))
	}
	s.configMu.Lock()
	sent, answered := s.configSent, s.configAnswer
	s.configSent = nil
	s.configMu.Unlock()
	if answered != nil && sent != nil {
		answered(sent, taken)
	}
	s.sendConfigure()
}

// keytermsRefusedWarningKey is the warning GET /api/status shows while
// Deepgram has refused the keyterms of a join or a leave.
const keytermsRefusedWarningKey = "deepgram-keyterms"

// keytermsRefusedWarning is the live keyterms' refusal hook for warnings
// (transcript.LiveKeyterms.SetRefusalHook).
func keytermsRefusedWarning(warnings *warningsRegistry) func(refused bool) {
	return func(refused bool) {
		if refused {
			warnings.Set(keytermsRefusedWarningKey, "Deepgram refused the live transcript's keyterms after a join or a leave, so it may not hear a joined agent's issue key; they go again with the next join, leave or reconnect")
			return
		}
		warnings.Clear(keytermsRefusedWarningKey)
	}
}

// isConfigureAnswer reports whether message is Deepgram's answer to a
// Configure, which the SDK's router does not know.
func isConfigureAnswer(message []byte) bool {
	if !bytes.Contains(message, []byte("Configure")) {
		return false
	}
	var head struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(message, &head) == nil && (head.Type == "ConfigureSuccess" || head.Type == "ConfigureFailure")
}

// Close tells Deepgram the stream ends, unless a write is in progress, which
// can be one stuck on a stalled peer, and closes the connection.
func (s *deepgramSocket) Close() error {
	if s.writeMu.TryLock() {
		if !s.closed.Load() {
			_ = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = s.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"CloseStream"}`))
		}
		s.writeMu.Unlock()
	}
	s.abort()
	<-s.done
	return nil
}

func (s *deepgramSocket) write(messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return errDeepgramSocketClosed
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil {
		s.abort()
		return err
	}
	if err := s.conn.WriteMessage(messageType, data); err != nil {
		s.abort()
		return fmt.Errorf("deepgram write: %w", err)
	}
	return nil
}

// abort closes the connection at once: a reset, so no unsent audio keeps the
// socket alive in the kernel. A write blocked on it returns.
func (s *deepgramSocket) abort() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	if tcp, ok := s.raw.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = s.conn.Close()
}

// logSocketEnd logs why Deepgram's socket ended: a close Deepgram sent by
// its code alone (the reason a close carries is Deepgram's text), any other
// read error reduced (providererr.From).
func logSocketEnd(err error) {
	var closed *websocket.CloseError
	if errors.As(err, &closed) {
		slog.Warn("deepgram error", "provider", "deepgram", "close_code", closed.Code)
		return
	}
	slog.Warn("deepgram error", "error", providererr.From("deepgram", err))
}

// read hands Deepgram's messages to the router until the connection ends,
// then reports the close. The socket logs its own read error here, rather
// than through the callback's Error, which logs only codes: an Error message
// Deepgram sends can set any of its fields.
func (s *deepgramSocket) read() {
	defer close(s.done)
	for {
		messageType, message, err := s.conn.ReadMessage()
		if err != nil {
			if !s.closed.Load() && !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				logSocketEnd(err)
			}
			s.abort()
			_ = s.router.Close(&msginterfaces.CloseResponse{Type: string(msginterfaces.TypeCloseResponse)})
			return
		}
		if messageType == websocket.TextMessage {
			if isConfigureAnswer(message) {
				s.configured(message)
				continue
			}
			_ = s.router.Message(message)
		}
	}
}

func (s *deepgramSocket) keepAlive() {
	ticker := time.NewTicker(deepgramKeepAliveEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if err := s.write(websocket.TextMessage, []byte(`{"type":"KeepAlive"}`)); err != nil {
				return
			}
		}
	}
}
