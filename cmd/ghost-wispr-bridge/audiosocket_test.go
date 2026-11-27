package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const audiosocketKey = "bridge-test-key"

// The secrets, hex of HMAC-SHA256 under audiosocketKey, made with
// `printf '%s' nonce-1 | openssl dgst -sha256 -hmac bridge-test-key`.
const (
	audiosocketSecret1        = "079d86f12e9f109daf585d58d03ea0e61fdb72b1b45a8a4dffc2eefa3f26a3c5" // nonce-1
	audiosocketSecret2        = "e33ea119a3a39c98e9de761c2a8a5f179f872100ad956f9f40f912629c1bf1a0" // nonce-2
	audiosocketSecretOtherKey = "9824b387b5b809ddd1d31a1958b9a40640ab59432a88f98c99a8336103d1107c" // nonce-1 under "another-key"
)

var audiosocketNow = time.Date(2026, 12, 1, 17, 0, 0, 0, time.UTC)

// audiosocketCert makes a certificate for localhost, as the program's CA
// gives the bridge, with its key in a file, and the pool that trusts it, as
// Attendee's worker does through SSL_CERT_FILE.
func audiosocketCert(t *testing.T) (certPEM, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyFile = filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), keyFile, pool
}

type audiosocketServer struct {
	addr string
	pool *x509.CertPool
}

// audiosocketServe serves sock over TLS on a free port of 127.0.0.1 until
// the test ends.
func audiosocketServe(t *testing.T, sock *audioSocket) audiosocketServer {
	t.Helper()
	certPEM, keyFile, pool := audiosocketCert(t)
	cert, err := loadAudioCert(certPEM, keyFile)
	if err != nil {
		t.Fatalf("loadAudioCert: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveAudioSocket(ctx, ln, cert, sock, newLogger(io.Discard, "bridge", slog.LevelDebug))
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serveAudioSocket: %v", err)
		}
	})
	return audiosocketServer{addr: ln.Addr().String(), pool: pool}
}

// audiosocketDial opens the socket as Attendee's bot does: wss to localhost,
// checking the certificate, with no Origin.
func audiosocketDial(t *testing.T, s audiosocketServer, path string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	d := websocket.Dialer{
		TLSClientConfig:  &tls.Config{RootCAs: s.pool, ServerName: "localhost", MinVersion: tls.VersionTLS12},
		HandshakeTimeout: 5 * time.Second,
	}
	conn, resp, err := d.Dial("wss://"+s.addr+path, nil)
	if conn != nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	return conn, resp, err
}

func audiosocketBase64(pcm []int16) string {
	b := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// audiosocketRamp is n samples from start, both extremes included when n is
// large enough.
func audiosocketRamp(n int, start int) []int16 {
	pcm := make([]int16, n)
	for i := range pcm {
		v := start + i*137
		pcm[i] = int16(v)
	}
	pcm[0], pcm[n-1] = 32767, -32768
	return pcm
}

func audiosocketReceive(t *testing.T, in <-chan audioIn) audioIn {
	t.Helper()
	select {
	case a := <-in:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("no audio came from the socket in 5 s")
		return audioIn{}
	}
}

// TestTheBridgeSendsOnlyAudioOnTheSocket holds that every message the bridge
// writes on Attendee's socket is a JSON text message with trigger
// realtime_audio.bot_output and data holding only chunk, base64 PCM16 of what
// the bridge sent, and sample_rate 24000. Attendee logs any other message it
// reads there at ERROR, the message included (bots/bot_controller/
// bot_controller.py:1946-1967). Attendee's own messages to the bridge, audio
// and otherwise, get no answer.
func TestTheBridgeSendsOnlyAudioOnTheSocket(t *testing.T) {
	in := make(chan audioIn, 16)
	sock := newAudioSocket([]byte(audiosocketKey), in, func() time.Time { return audiosocketNow })
	sock.Register("nonce-1")
	srv := audiosocketServe(t, sock)
	conn, _, err := audiosocketDial(t, srv, audioSocketPath+audiosocketSecret1)
	if err != nil {
		t.Fatalf("dial the registered bot's socket: %v", err)
	}
	attendeeSays := []string{
		`{"trigger": "realtime_audio.mixed", "bot_id": "bot_1", "data": {"chunk": "` + audiosocketBase64(audiosocketRamp(240, 0)) + `", "timestamp_ms": 1764608400000, "sample_rate": 24000}}`,
		`{"trigger": "realtime_audio.per_participant", "bot_id": "bot_1", "data": {"participant_uuid": "spaces/s/devices/7", "chunk": "` + audiosocketBase64(audiosocketRamp(160, 5)) + `", "timestamp_ms": 1764608400010, "sample_rate": 16000}}`,
		`{"trigger": "realtime_video.per_participant", "bot_id": "bot_1", "data": {"frame": "", "format": "jpeg"}}`,
		`not json`,
	}
	for _, m := range attendeeSays {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	audiosocketReceive(t, in)
	audiosocketReceive(t, in)

	frames := [][]int16{audiosocketRamp(480, 1), audiosocketRamp(480, -9000), make([]int16, 480)}
	for i, f := range frames {
		if err := sock.SendBotOutput(f); err != nil {
			t.Fatalf("SendBotOutput %d: %v", i, err)
		}
	}

	// Everything the bridge wrote, until it is quiet for 300 ms.
	type message struct {
		kind int
		data []byte
	}
	var got []message
	for {
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		kind, data, err := conn.ReadMessage()
		if err != nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				t.Fatalf("read from the bridge: %v", err)
			}
			break
		}
		got = append(got, message{kind, data})
	}

	var sent [][]int16
	for i, m := range got {
		if m.kind != websocket.TextMessage {
			t.Errorf("message %d is not a text message (type %d): %q", i, m.kind, m.data)
			continue
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(m.data, &top); err != nil {
			t.Errorf("message %d is not a JSON object: %s", i, m.data)
			continue
		}
		var data map[string]json.RawMessage
		_ = json.Unmarshal(top["data"], &data)
		var trigger, chunk string
		var rate int
		_ = json.Unmarshal(top["trigger"], &trigger)
		_ = json.Unmarshal(data["chunk"], &chunk)
		_ = json.Unmarshal(data["sample_rate"], &rate)
		if len(top) != 2 || trigger != "realtime_audio.bot_output" || len(data) != 2 || chunk == "" || rate != 24000 {
			t.Errorf("message %d is not bot audio at 24000 alone: %s", i, m.data)
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(chunk)
		if err != nil || len(raw)%2 != 0 {
			t.Errorf("message %d's chunk is not base64 PCM16: %s", i, m.data)
			continue
		}
		pcm := make([]int16, len(raw)/2)
		for j := range pcm {
			pcm[j] = int16(binary.LittleEndian.Uint16(raw[2*j:]))
		}
		sent = append(sent, pcm)
	}
	if len(got) != len(frames) {
		t.Errorf("the bridge wrote %d messages, want the %d frames it sent", len(got), len(frames))
	}
	if !slices.EqualFunc(sent, frames, slices.Equal[[]int16]) {
		t.Errorf("the chunks the bridge wrote are not the frames it was given, in order")
	}
}

// TestTheAudioSocketTakesOnlyARegisteredBotsSecret holds that the socket
// takes a connection only on /attendee/audio/<the secret of a registered
// nonce>, also one registered after the server started, as after a restart,
// and that Connected tells whether such a connection is open.
func TestTheAudioSocketTakesOnlyARegisteredBotsSecret(t *testing.T) {
	in := make(chan audioIn, 16)
	sock := newAudioSocket([]byte(audiosocketKey), in, func() time.Time { return audiosocketNow })
	sock.Register("nonce-1")
	srv := audiosocketServe(t, sock)
	if sock.Connected() {
		t.Fatal("Connected before any connection")
	}
	if err := sock.SendBotOutput(make([]int16, 480)); err == nil {
		t.Error("SendBotOutput with no connection returned no error")
	}
	for _, path := range []string{
		audioSocketPath + audiosocketSecret2,        // a nonce not registered
		audioSocketPath + audiosocketSecretOtherKey, // a registered nonce under another key
		audioSocketPath,
		audioSocketPath + audiosocketSecret1 + "/x",
		audioSocketPath + audiosocketSecret1[:63],
		"/attendee/webhook",
	} {
		conn, resp, err := audiosocketDial(t, srv, path)
		if err == nil {
			_ = conn.Close()
			t.Errorf("the socket took %s", path)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusNotFound {
			t.Errorf("dial %s: %v, want 404", path, err)
		}
	}
	if sock.Connected() {
		t.Fatal("Connected after refusals only")
	}

	sock.Register("nonce-2")
	conn, _, err := audiosocketDial(t, srv, audioSocketPath+audiosocketSecret2)
	if err != nil {
		t.Fatalf("dial a nonce registered after the start: %v", err)
	}
	clockWaitFor(t, sock.Connected, "Connected after the connection")
	if err := sock.SendBotOutput(make([]int16, 480)); err != nil {
		t.Errorf("SendBotOutput on the connection: %v", err)
	}
	_ = conn.Close()
	clockWaitFor(t, func() bool { return !sock.Connected() }, "not Connected after the close")
}

// One call at a time, one bot's socket (PC14): the registration of a newer
// bot retires the older bot's secret and closes its connection, so the older
// bot's reconnect is refused and cannot stand in for the newer bot's socket.
func TestOnlyTheCurrentBotsSocketIsTaken(t *testing.T) {
	in := make(chan audioIn, 16)
	sock := newAudioSocket([]byte(audiosocketKey), in, func() time.Time { return audiosocketNow })
	sock.Register("nonce-1")
	srv := audiosocketServe(t, sock)
	older, _, err := audiosocketDial(t, srv, audioSocketPath+audiosocketSecret1)
	if err != nil {
		t.Fatalf("dial the first bot's socket: %v", err)
	}
	clockWaitFor(t, sock.Connected, "Connected with the first bot's socket")

	sock.Register("nonce-2")
	if sock.Connected() {
		t.Fatal("Connected while only the older bot's socket is open")
	}
	_ = older.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ne net.Error
	if _, _, err := older.ReadMessage(); err == nil {
		t.Fatal("the older bot's connection carried a message after the newer bot's registration")
	} else if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the older bot's connection stays open after the newer bot's registration")
	}
	if conn, resp, err := audiosocketDial(t, srv, audioSocketPath+audiosocketSecret1); err == nil {
		_ = conn.Close()
		t.Fatal("the socket took the older bot's reconnect")
	} else if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the older bot's reconnect: %v, want 404", err)
	}
	newer, _, err := audiosocketDial(t, srv, audioSocketPath+audiosocketSecret2)
	if err != nil {
		t.Fatalf("dial the newer bot's socket: %v", err)
	}
	clockWaitFor(t, sock.Connected, "Connected with the newer bot's socket")
	mixed := `{"trigger": "realtime_audio.mixed", "bot_id": "bot_2", "data": {"chunk": "` + audiosocketBase64(audiosocketRamp(240, 0)) + `", "sample_rate": 24000}}`
	if err := newer.WriteMessage(websocket.TextMessage, []byte(mixed)); err != nil {
		t.Fatal(err)
	}
	if a := audiosocketReceive(t, in); a.Nonce != "nonce-2" {
		t.Fatalf("the newer bot's audio names nonce %q, want nonce-2", a.Nonce)
	}
	// The bot's end retires its secret and closes its connection.
	sock.Retire("nonce-2")
	if sock.Connected() {
		t.Fatal("Connected after the newer bot was retired")
	}
	_ = newer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := newer.ReadMessage(); err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the retired bot's connection stays open: %v", err)
	}
}

// TestTheAudioSocketDecodesMixedAndPerParticipantAudio holds that each audio
// message of Attendee (bots/websocket_payloads.py:35-67) comes out as one
// audioIn with its PCM, its kind, its participant's Meet id and the bridge's
// time of receipt, and that a chunk of an odd byte count and a message of
// another trigger give nothing.
func TestTheAudioSocketDecodesMixedAndPerParticipantAudio(t *testing.T) {
	in := make(chan audioIn, 16)
	sock := newAudioSocket([]byte(audiosocketKey), in, func() time.Time { return audiosocketNow })
	sock.Register("nonce-1")
	srv := audiosocketServe(t, sock)
	conn, _, err := audiosocketDial(t, srv, audioSocketPath+audiosocketSecret1)
	if err != nil {
		t.Fatal(err)
	}
	mixed1, voice, mixed2 := audiosocketRamp(240, 3), audiosocketRamp(160, -40), audiosocketRamp(240, 900)
	for _, m := range []string{
		`{"trigger": "realtime_audio.mixed", "bot_id": "bot_1", "data": {"chunk": "` + audiosocketBase64(mixed1) + `", "timestamp_ms": 1764608400000, "sample_rate": 24000}}`,
		`{"trigger": "realtime_audio.mixed", "bot_id": "bot_1", "data": {"chunk": "AAAA", "timestamp_ms": 1764608400005, "sample_rate": 24000}}`, // 3 bytes: not PCM16
		`{"trigger": "realtime_audio.per_participant", "bot_id": "bot_1", "data": {"participant_uuid": "spaces/s/devices/7", "chunk": "` + audiosocketBase64(voice) + `", "timestamp_ms": 1764608400010, "sample_rate": 16000}}`,
		`{"trigger": "realtime_video.per_participant", "bot_id": "bot_1", "data": {"frame": "", "format": "jpeg", "participant_uuid": "spaces/s/devices/7", "source": "webcam"}}`,
		`{"trigger": "realtime_audio.mixed", "bot_id": "bot_1", "data": {"chunk": "` + audiosocketBase64(mixed2) + `", "timestamp_ms": 1764608400020, "sample_rate": 24000}}`,
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	want := []audioIn{
		{Mixed: true, PCM: mixed1, At: audiosocketNow},
		{Mixed: false, ParticipantID: "spaces/s/devices/7", PCM: voice, At: audiosocketNow},
		{Mixed: true, PCM: mixed2, At: audiosocketNow},
	}
	for i, w := range want {
		g := audiosocketReceive(t, in)
		if g.Mixed != w.Mixed || g.ParticipantID != w.ParticipantID || !slices.Equal(g.PCM, w.PCM) || !g.At.Equal(w.At) {
			t.Errorf("audio %d: got {Mixed:%t ParticipantID:%q PCM:%d samples At:%s}, want {Mixed:%t ParticipantID:%q PCM:%d samples At:%s}",
				i, g.Mixed, g.ParticipantID, len(g.PCM), g.At, w.Mixed, w.ParticipantID, len(w.PCM), w.At)
		}
	}
	select {
	case a := <-in:
		t.Errorf("an extra audioIn came: %d samples", len(a.PCM))
	case <-time.After(100 * time.Millisecond):
	}
}
