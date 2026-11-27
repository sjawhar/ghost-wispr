package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// audioSocketPath is the audio socket's path; the bot's socket secret
// follows it.
const audioSocketPath = "/attendee/audio/"

// audioSocketReadLimit bounds one of Attendee's messages: 10 ms of audio,
// about 1 KB (bots/websocket_payloads.py:35-67).
const audioSocketReadLimit = 64 << 10

// audioSocketWriteWait bounds one write to Attendee's bot.
const audioSocketWriteWait = 2 * time.Second

// audioIn is one audio message from Attendee's bot.
type audioIn struct {
	Mixed         bool   // realtime_audio.mixed (24 kHz); false: per_participant (16 kHz)
	ParticipantID string // per_participant only
	PCM           []int16
	At            time.Time // receive time on the bridge's clock
	Nonce         string    // the nonce of the bot whose connection carried it
}

// errAudioSocketDown is SendBotOutput's error while no bot is connected.
var errAudioSocketDown = errors.New("no bot is connected to the audio socket")

// audioSocket is the socket Attendee's bot dials, once for its mixed and its
// per-participant audio (bots/bot_controller/bot_websocket_client_manager.py:
// 9-44, one client for equal URLs). It takes a connection only on the socket
// secret of the one nonce registered with Register, the call's bot's (one
// call at a time), sends each audio message it reads on its channel with
// that nonce, and writes nothing but realtime_audio.bot_output messages:
// Attendee logs any other message at ERROR, the message included
// (bots/bot_controller/bot_controller.py:1946-1967).
type audioSocket struct {
	key      []byte
	in       chan<- audioIn
	now      func() time.Time
	upgrader websocket.Upgrader

	mu     sync.Mutex
	nonce  string             // the call's bot's; "" when none
	secret string             // its socket secret
	conn   *websocket.Conn    // its connection; nil while none is open
	cancel context.CancelFunc // releases a reader blocked on the audio consumer

	writeMu sync.Mutex // one writer at a time, and the buffers below
	pcm     []byte
	out     []byte
}

// newAudioSocket makes the socket. bridgeKey is BRIDGE_KEY; in gets each
// audio message, and the socket blocks on it; now is the bridge's clock.
func newAudioSocket(bridgeKey []byte, in chan<- audioIn, now func() time.Time) *audioSocket {
	return &audioSocket{key: bridgeKey, in: in, now: now}
}

// Register takes the socket of the bot of nonce alone: the bridge calls it
// for each bot it creates, and for the call's bot a restarted bridge finds.
// An other bot's secret is retired and its connection closed.
func (s *audioSocket) Register(nonce string) {
	secret := socketSecret(s.key, nonce)
	s.mu.Lock()
	if s.nonce == nonce {
		s.mu.Unlock()
		return
	}
	s.nonce, s.secret = nonce, secret
	conn, cancel := s.conn, s.cancel
	s.conn, s.cancel = nil, nil
	s.mu.Unlock()
	closeAudioConn(conn, cancel)
}

// Retire takes the socket of the bot of nonce no more, and closes its
// connection: the bridge calls it at the bot's end.
func (s *audioSocket) Retire(nonce string) {
	s.mu.Lock()
	if nonce == "" || s.nonce != nonce {
		s.mu.Unlock()
		return
	}
	s.nonce, s.secret = "", ""
	conn, cancel := s.conn, s.cancel
	s.conn, s.cancel = nil, nil
	s.mu.Unlock()
	closeAudioConn(conn, cancel)
}

// Connected tells whether the registered bot's socket is open.
func (s *audioSocket) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// taken gives the registered bot's nonce when secret is its socket secret.
func (s *audioSocket) taken(secret string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret == "" || subtle.ConstantTimeCompare([]byte(s.secret), []byte(secret)) != 1 {
		return "", false
	}
	return s.nonce, true
}

// ServeHTTP takes the registered bot's socket on audioSocketPath + its
// secret. A newer connection of that bot replaces an older one: Attendee
// dials again after a failure.
func (s *audioSocket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secret, ok := strings.CutPrefix(r.URL.Path, audioSocketPath)
	if !ok || secret == "" {
		http.NotFound(w, r)
		return
	}
	nonce, ok := s.taken(secret)
	if !ok {
		http.NotFound(w, r)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(audioSocketReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.mu.Lock()
	if s.nonce != nonce { // the bot was retired during the handshake
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	old, oldCancel := s.conn, s.cancel
	s.conn, s.cancel = conn, cancel
	s.mu.Unlock()
	closeAudioConn(old, oldCancel)
	s.read(ctx, conn, nonce)
	s.mu.Lock()
	if s.conn == conn {
		s.conn, s.cancel = nil, nil
	}
	s.mu.Unlock()
	_ = conn.Close()
}

// closeAudioConn closes a connection the socket no longer holds, and
// releases its reader.
func closeAudioConn(conn *websocket.Conn, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// attendeeAudioMessage is the part of Attendee's audio message the bridge
// reads (bots/websocket_payloads.py:35-67). The raw fields keep their
// buffers from one message to the next.
type attendeeAudioMessage struct {
	Trigger json.RawMessage `json:"trigger"`
	Data    struct {
		Chunk           json.RawMessage `json:"chunk"`
		ParticipantUUID string          `json:"participant_uuid"`
	} `json:"data"`
}

var (
	audioTriggerMixed          = []byte(`"realtime_audio.mixed"`)
	audioTriggerPerParticipant = []byte(`"realtime_audio.per_participant"`)
)

// read sends each audio message of conn, the connection of the bot of
// nonce, on s.in until conn fails or ctx ends; it drops every other message.
func (s *audioSocket) read(ctx context.Context, conn *websocket.Conn, nonce string) {
	var (
		buf     bytes.Buffer
		msg     attendeeAudioMessage
		decoded []byte
	)
	for {
		_, r, err := conn.NextReader()
		if err != nil {
			return
		}
		buf.Reset()
		if _, err := buf.ReadFrom(r); err != nil {
			return
		}
		msg.Trigger, msg.Data.Chunk, msg.Data.ParticipantUUID = msg.Trigger[:0], msg.Data.Chunk[:0], ""
		if err := json.Unmarshal(buf.Bytes(), &msg); err != nil {
			continue
		}
		mixed := bytes.Equal(msg.Trigger, audioTriggerMixed)
		if !mixed && !bytes.Equal(msg.Trigger, audioTriggerPerParticipant) {
			continue
		}
		chunk := msg.Data.Chunk
		if len(chunk) < 2 || chunk[0] != '"' || chunk[len(chunk)-1] != '"' {
			continue
		}
		chunk = chunk[1 : len(chunk)-1]
		decoded = decoded[:cap(decoded)]
		if n := base64.StdEncoding.DecodedLen(len(chunk)); n > len(decoded) {
			decoded = make([]byte, n)
		}
		n, err := base64.StdEncoding.Decode(decoded, chunk)
		if err != nil || n%2 != 0 {
			continue
		}
		pcm := make([]int16, n/2)
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(decoded[2*i:]))
		}
		a := audioIn{Mixed: mixed, PCM: pcm, At: s.now(), Nonce: nonce}
		if !mixed {
			a.ParticipantID = msg.Data.ParticipantUUID
		}
		select {
		case s.in <- a:
		case <-ctx.Done():
			return
		}
	}
}

// SendBotOutput sends pcm, 24 kHz PCM16 mono, to the bot's microphone as one
// realtime_audio.bot_output message (Attendee's realtime audio docs;
// bots/bot_controller/bot_controller.py:1949-1952). It is an error while no
// bot is connected; a failed write closes the connection.
func (s *audioSocket) SendBotOutput(pcm []int16) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return errAudioSocketDown
	}
	s.pcm = s.pcm[:0]
	for _, v := range pcm {
		s.pcm = binary.LittleEndian.AppendUint16(s.pcm, uint16(v))
	}
	s.out = append(s.out[:0], `{"trigger":"realtime_audio.bot_output","data":{"chunk":"`...)
	s.out = base64.StdEncoding.AppendEncode(s.out, s.pcm)
	s.out = append(s.out, `","sample_rate":`...)
	s.out = strconv.AppendInt(s.out, botMixedRate, 10)
	s.out = append(s.out, "}}"...)
	_ = conn.SetWriteDeadline(time.Now().Add(audioSocketWriteWait))
	if err := conn.WriteMessage(websocket.TextMessage, s.out); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write bot output: %w", err)
	}
	return nil
}

// closeConn closes the open connection, if any.
func (s *audioSocket) closeConn() {
	s.mu.Lock()
	conn, cancel := s.conn, s.cancel
	s.conn, s.cancel = nil, nil
	s.mu.Unlock()
	closeAudioConn(conn, cancel)
}

// loadAudioCert reads the audio socket's certificate: certPEM is
// BRIDGE_TLS_CERT, the program CA's certificate for localhost, and keyFile is
// BRIDGE_TLS_KEY_FILE. Attendee takes only wss:// addresses and checks the
// certificate (bots/serializers.py:1623-1629).
func loadAudioCert(certPEM, keyFile string) (*tls.Certificate, error) {
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read the audio socket's key: %w", err)
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), key)
	if err != nil {
		return nil, fmt.Errorf("the audio socket's certificate and key: %w", err)
	}
	return &cert, nil
}

// serveAudioSocket serves s over TLS with cert on ln until ctx ends, then
// closes the bot's connection and the server.
func serveAudioSocket(ctx context.Context, ln net.Listener, cert *tls.Certificate, s *audioSocket, log *slog.Logger) error {
	mux := http.NewServeMux()
	mux.Handle("GET "+audioSocketPath, s)
	srv := newServer(ctx, "audio_socket", mux, log)
	srv.TLSConfig = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{*cert},
		NextProtos:   []string{"http/1.1"}, // a WebSocket needs HTTP/1.1
	}
	srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	// A WebSocket is a hijacked connection, which Shutdown leaves open.
	srv.RegisterOnShutdown(s.closeConn)
	return serveUntil(ctx, srv, func() error { return srv.ServeTLS(ln, "", "") }, log, "audio_socket")
}
