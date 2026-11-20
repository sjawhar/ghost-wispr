package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	// ended says CloseStream went out on a graceful close: every write after
	// it fails, so audio sent after it goes to the next socket (CloseGracefully).
	ended atomic.Bool
	done  chan struct{}
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

// CloseGracefully ends the stream as Deepgram documents it (Close Stream):
// CloseStream makes Deepgram process the audio it has, return its final
// results, and close the connection. It waits for that close, or wait, before
// closing the connection itself, and the final results reach the callback
// meanwhile, so the audio this socket accepted is transcribed. A write in
// progress is sent first (the write lock). It replaces a healthy socket after
// an options change (transcribe.GracefulCloser); Close ends a failed one.
func (s *deepgramSocket) CloseGracefully(wait time.Duration) error {
	if s.endStream() {
		timer := time.NewTimer(wait)
		select {
		case <-s.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	s.abort()
	<-s.done
	return nil
}

// endStream sends CloseStream and makes every later write fail, under the
// write lock: a write in progress goes out first, and audio a writer sends
// after it would reach a stream Deepgram has finished, so it fails instead and
// the client sends it on the next socket. It reports whether CloseStream went
// out.
func (s *deepgramSocket) endStream() bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() || s.ended.Swap(true) {
		return false
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil {
		s.abort()
		return false
	}
	if err := s.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"CloseStream"}`)); err != nil {
		s.abort()
		return false
	}
	return true
}

func (s *deepgramSocket) write(messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() || s.ended.Load() {
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
