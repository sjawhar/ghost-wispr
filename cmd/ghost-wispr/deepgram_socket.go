package main

import (
	"context"
	"errors"
	"fmt"
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
}

// dialDeepgramLive opens a live-transcription socket to Deepgram with
// cOptions' host and credentials and tOptions' query, as the SDK's client
// would.
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
	url, err := version.GetLiveAPI(ctx, cOptions.Host, cOptions.APIVersion, cOptions.Path, tOptions)
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
	tcp := &net.Dialer{Timeout: 15 * time.Second, Control: tcpUserTimeout(timeout)}
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
		return nil, fmt.Errorf("deepgram connect: %w", err)
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

// read hands Deepgram's messages to the router until the connection ends,
// then reports the close.
func (s *deepgramSocket) read() {
	defer close(s.done)
	for {
		messageType, message, err := s.conn.ReadMessage()
		if err != nil {
			if !s.closed.Load() && !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				_ = s.router.Error(&msginterfaces.ErrorResponse{
					Type:        string(msginterfaces.TypeErrorResponse),
					ErrMsg:      "socket",
					Description: err.Error(),
				})
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
