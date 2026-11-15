package main

// The live-transcription protocol of the app-owned Deepgram socket
// (deepgramSocket), against a fake Deepgram server that records what the
// socket sends and answers with Deepgram's own message shapes. Sessions start
// only from live transcripts, so a socket that drops them, or that Deepgram
// closes for want of a KeepAlive, leaves the unit saving nothing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"
)

// deepgramEndpoint is a Deepgram live endpoint that takes one connection.
type deepgramEndpoint struct {
	mu     sync.Mutex
	header http.Header
	query  url.Values
	types  []string // the "type" of each text frame read
	conn   chan *websocket.Conn
	ended  chan struct{} // closed when the connection ends
}

func startDeepgramEndpoint(t *testing.T) (*deepgramEndpoint, string) {
	t.Helper()
	fake := &deepgramEndpoint{conn: make(chan *websocket.Conn, 1), ended: make(chan struct{})}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.header, fake.query = r.Header.Clone(), r.URL.Query()
		fake.mu.Unlock()
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer close(fake.ended)
		defer func() { _ = c.Close() }()
		fake.conn <- c
		for {
			kind, message, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage {
				continue
			}
			var frame struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(message, &frame); err != nil {
				frame.Type = "unparsable: " + string(message)
			}
			fake.mu.Lock()
			fake.types = append(fake.types, frame.Type)
			fake.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	return fake, "ws://" + srv.Listener.Addr().String()
}

func (f *deepgramEndpoint) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.types)
}

func (f *deepgramEndpoint) waitFor(t *testing.T, within time.Duration, messageType string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if slices.Contains(f.received(), messageType) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Deepgram got %q within %v, want a %s message", f.received(), within, messageType)
}

// recordingManager is the session manager's side of live transcription.
type recordingManager struct {
	mu            sync.Mutex
	finals        []string
	utteranceEnds int
}

func (m *recordingManager) Message(mr *api.MessageResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mr.IsFinal && len(mr.Channel.Alternatives) > 0 {
		m.finals = append(m.finals, mr.Channel.Alternatives[0].Transcript)
	}
	return nil
}

func (m *recordingManager) UtteranceEnd(*api.UtteranceEndResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.utteranceEnds++
	return nil
}

func (m *recordingManager) ForceEndSession(context.Context) error { return nil }

func dialDeepgramEndpoint(t *testing.T, host string, manager *recordingManager) *deepgramSocket {
	t.Helper()
	callback := transcriptCallback{}
	if manager != nil {
		callback.manager = manager
	}
	socket, err := dialDeepgramLive(context.Background(), "test-key",
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		callback, time.Second)
	if err != nil {
		t.Fatalf("dial the fake Deepgram: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	return socket
}

// TestDeepgramSocketSendsKeepAliveWhileIdle fails if an idle socket sends no
// KeepAlive: Deepgram closes a socket that gets no audio and no KeepAlive for
// about 10 s, as it does across a pause in speech.
func TestDeepgramSocketSendsKeepAliveWhileIdle(t *testing.T) {
	fake, host := startDeepgramEndpoint(t)
	dialDeepgramEndpoint(t, host, nil)
	fake.waitFor(t, deepgramKeepAliveEvery+2*time.Second, "KeepAlive")
}

// TestDeepgramSocketFinalizeSendsFinalize fails if Finalize, which a capture
// pause sends so Deepgram returns its last results, is not Deepgram's
// Finalize message.
func TestDeepgramSocketFinalizeSendsFinalize(t *testing.T) {
	fake, host := startDeepgramEndpoint(t)
	socket := dialDeepgramEndpoint(t, host, nil)
	if err := socket.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	fake.waitFor(t, 2*time.Second, "Finalize")
}

// TestDeepgramSocketCloseSendsCloseStream fails if closing the socket does not
// tell Deepgram the stream ends with CloseStream before the connection goes.
func TestDeepgramSocketCloseSendsCloseStream(t *testing.T) {
	fake, host := startDeepgramEndpoint(t)
	socket := dialDeepgramEndpoint(t, host, nil)
	if err := socket.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-fake.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the connection did not end after Close")
	}
	if got := fake.received(); !slices.Contains(got, "CloseStream") {
		t.Fatalf("Deepgram got %q before the connection ended, want CloseStream", got)
	}
}

// TestDeepgramTranscriptsReachTheSessionManager fails if Deepgram's results
// and utterance ends do not reach the session manager, which starts sessions
// from final transcripts, or if the socket does not ask Deepgram with the
// key and the options it was given.
func TestDeepgramTranscriptsReachTheSessionManager(t *testing.T) {
	fake, host := startDeepgramEndpoint(t)
	manager := &recordingManager{}
	dialDeepgramEndpoint(t, host, manager)
	var conn *websocket.Conn
	select {
	case conn = <-fake.conn:
	case <-time.After(2 * time.Second):
		t.Fatal("the fake Deepgram got no connection")
	}

	fake.mu.Lock()
	auth, query := fake.header.Get("Authorization"), fake.query
	fake.mu.Unlock()
	if auth != "token test-key" || query.Get("model") != "nova-3" || query.Get("encoding") != "linear16" || query.Get("sample_rate") != "16000" {
		t.Fatalf("Deepgram was asked with Authorization %q and query %v, want the key and the options", auth, query)
	}

	for _, frame := range []string{
		`{"type":"SpeechStarted","channel":[0,1],"timestamp":0.1}`,
		`{"type":"Results","channel_index":[0,1],"duration":1.5,"start":0,"is_final":false,"channel":{"alternatives":[{"transcript":"Hello","confidence":0.8}]}}`,
		`{"type":"Results","channel_index":[0,1],"duration":1.5,"start":0,"is_final":true,"speech_final":true,"channel":{"alternatives":[{"transcript":"Hello world.","confidence":0.9,"words":[{"word":"hello","start":0.1,"end":0.5,"confidence":0.9,"punctuated_word":"Hello"},{"word":"world","start":0.6,"end":1.0,"confidence":0.9,"punctuated_word":"world."}]}]}}`,
		`{"type":"UtteranceEnd","channel":[0,1],"last_word_end":1.0}`,
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		done := len(manager.finals) == 1 && manager.utteranceEnds == 1
		manager.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !slices.Equal(manager.finals, []string{"Hello world."}) || manager.utteranceEnds != 1 {
		t.Fatalf("the session manager got finals %q and %d utterance ends, want [\"Hello world.\"] and 1", manager.finals, manager.utteranceEnds)
	}
}
