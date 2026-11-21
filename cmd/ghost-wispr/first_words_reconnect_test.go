package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// reconnectBytesPerSecond is the byte rate of the audio this test captures:
// 16-bit mono PCM at 16000 Hz.
const reconnectBytesPerSecond = 16000 * 2

// reconnectSecond returns one second of audio whose samples all hold label, so
// the test can tell which second of the capture a stream or a recording holds.
func reconnectSecond(label uint16) []byte {
	pcm := make([]byte, reconnectBytesPerSecond)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], label)
	}
	return pcm
}

// listeningDeepgram is a Deepgram live endpoint that refuses handshakes with
// HTTP 503 while refusing is set, and otherwise keeps every connection with
// the audio it has read from it, so the test can answer each with results
// timed in that connection's own stream, as Deepgram does.
type listeningDeepgram struct {
	refusing atomic.Bool
	mu       sync.Mutex
	conns    []*listenedConn
}

type listenedConn struct {
	conn  *websocket.Conn
	mu    sync.Mutex
	audio []byte
}

func startListeningDeepgram(t *testing.T) (*listeningDeepgram, string) {
	t.Helper()
	peer := &listeningDeepgram{}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if peer.refusing.Load() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		listened := &listenedConn{conn: c}
		peer.mu.Lock()
		peer.conns = append(peer.conns, listened)
		peer.mu.Unlock()
		for {
			kind, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				listened.mu.Lock()
				listened.audio = append(listened.audio, data...)
				listened.mu.Unlock()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return peer, "ws://" + srv.Listener.Addr().String()
}

// latest returns the last connection the peer accepted, or nil.
func (p *listeningDeepgram) latest() *listenedConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.conns) == 0 {
		return nil
	}
	return p.conns[len(p.conns)-1]
}

// dropAll closes every connection the peer holds.
func (p *listeningDeepgram) dropAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.conn.Close()
	}
}

// heardAt returns where in the connection's stream the first sample labelled
// label is, in seconds, as Deepgram would time a word spoken there.
func (c *listenedConn) heardAt(label uint16) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for at := 0; at+2 <= len(c.audio); at += 2 {
		if binary.LittleEndian.Uint16(c.audio[at:]) == label {
			return float64(at) / reconnectBytesPerSecond, true
		}
	}
	return 0, false
}

func (c *listenedConn) streamSeconds() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return float64(len(c.audio)) / reconnectBytesPerSecond
}

// result sends a Deepgram live result for the window from start, of
// duration seconds, with one word a second from first.
func (c *listenedConn) result(t *testing.T, isFinal, speechFinal bool, start, duration, first float64, words ...string) {
	t.Helper()
	type word struct {
		Word           string  `json:"word"`
		PunctuatedWord string  `json:"punctuated_word"`
		Start          float64 `json:"start"`
		End            float64 `json:"end"`
		Confidence     float64 `json:"confidence"`
		Speaker        int     `json:"speaker"`
	}
	timed := make([]word, len(words))
	for i, w := range words {
		at := first + float64(i)
		timed[i] = word{Word: strings.ToLower(w), PunctuatedWord: w, Start: at, End: at + 0.5, Confidence: 0.9}
	}
	frame, err := json.Marshal(map[string]any{
		"type":          "Results",
		"channel_index": []int{0, 1},
		"start":         start,
		"duration":      duration,
		"is_final":      isFinal,
		"speech_final":  speechFinal,
		"channel": map[string]any{"alternatives": []map[string]any{{
			"transcript": strings.Join(words, " "),
			"confidence": 0.9,
			"words":      timed,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("send a result: %v", err)
	}
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordedLabels returns the labels of the seconds a raw recording holds.
func recordedLabels(t *testing.T, path string) []uint16 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw)%reconnectBytesPerSecond != 0 {
		t.Fatalf("the recording holds %d bytes, not whole captured seconds", len(raw))
	}
	var labels []uint16
	for at := 0; at < len(raw); at += reconnectBytesPerSecond {
		second := raw[at : at+reconnectBytesPerSecond]
		label := binary.LittleEndian.Uint16(second)
		if !bytes.Equal(second, reconnectSecond(label)) {
			t.Fatalf("second %d of the recording is not one captured second", at/reconnectBytesPerSecond)
		}
		labels = append(labels, label)
	}
	return labels
}

// outageRecording runs the real manager, recorder and live client against a
// peer that times its results in each socket's own stream, as Deepgram does:
// room seconds of audio (labels 1, 2, ...) reach a first socket; the peer
// drops it and refuses new ones, and the live client buffers the audio,
// first some probe seconds (label 3) until it sees the socket gone, then
// speech seconds (labels from 10); the peer accepts again, the client sends
// what it buffered, and latency seconds more are captured before the peer's
// first result for the words that began at label 10. It returns the labels
// of the seconds the session the words open records first.
func outageRecording(t *testing.T, room, speech, latency int) []uint16 {
	t.Helper()
	peer, host := startListeningDeepgram(t)
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	audioDir := t.TempDir()
	recorder := audio.NewRecorder(audioDir)
	manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{SampleRate: 16000, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		transcriptCallback{manager: manager}, newWarningsRegistry(), nil)
	t.Cleanup(func() { _ = rc.Close() })
	mic := recorder.Writer(rc)
	say := func(labels ...uint16) {
		t.Helper()
		for _, label := range labels {
			if _, err := mic.Write(reconnectSecond(label)); err != nil {
				t.Fatalf("capture second %d: %v", label, err)
			}
		}
	}
	sayFrom := func(first uint16, n int) uint16 {
		t.Helper()
		for range n {
			say(first)
			first++
		}
		return first
	}

	waitUntil(t, "the first socket", func() bool { return rc.IsConnected() && peer.latest() != nil })
	first := peer.latest()
	sayFrom(1, room) // the room before anyone speaks
	waitUntil(t, "the first socket to get its audio", func() bool { return first.streamSeconds() >= float64(room) })

	// Deepgram drops the socket and refuses the next ones: the room's audio
	// waits in the live client.
	peer.refusing.Store(true)
	peer.dropAll()
	waitUntil(t, "the live client to see the socket gone", func() bool {
		say(3)
		return !rc.IsConnected()
	})
	next := sayFrom(10, speech) // "Good morning, everyone. ..."

	// Deepgram is back: the live client sends what it buffered to a new
	// socket, then the room's audio as it comes.
	peer.refusing.Store(false)
	waitUntil(t, "a second socket", func() bool { return peer.latest() != first })
	second := peer.latest()
	waitUntil(t, "the buffered audio to reach the second socket", func() bool {
		at, ok := second.heardAt(10)
		return ok && second.streamSeconds() >= at+float64(speech)
	})
	sayFrom(next, latency)
	spoken, _ := second.heardAt(10)

	// Deepgram's results for the second socket's stream, timed in it.
	second.result(t, true, false, 0, spoken, 0)
	second.result(t, false, false, spoken, 2, spoken, "Good", "morning,")
	second.result(t, true, true, spoken, float64(speech), spoken, "Good", "morning,", "everyone.")
	waitUntil(t, "the first words to open a session", func() bool {
		id, _ := manager.ActiveSession()
		return id != ""
	})
	// The session is current before its recording starts: wait for the start.
	manager.WaitForSessionStarts()
	id, _ := manager.ActiveSession()
	return recordedLabels(t, recording.RawPath(audioDir, id))
}

// opensAPrerollBefore fails the test unless the recording holds exactly
// prerollSeconds (5 s) before label 10, the first words, and every second
// from them on.
func opensAPrerollBefore(t *testing.T, got []uint16, speech, latency int) {
	t.Helper()
	var words []uint16
	for label := uint16(10); label < uint16(10+speech+latency); label++ {
		words = append(words, label)
	}
	if len(got) != 5+len(words) || !slices.Equal(got[5:], words) {
		t.Fatalf("the recording holds %d seconds, opening with %v; want exactly 5 s, then seconds 10 to %d, from the first words the live transcript holds", len(got), got[:min(8, len(got))], 10+speech+latency-1)
	}
}

// TestFirstWordsDeepgramHearsAfterAReconnectAreAllInTheRecording fails if a
// session's recording does not open 5 s before the first words its live
// transcript holds when Deepgram transcribes them late. While Deepgram is not
// connected, the live client buffers the microphone's audio (up to 60 s) and
// sends it once a socket connects again; Deepgram's results for it come only
// then. The recording kept only the last 5 s before those results came back,
// so the first words of a meeting that began during the outage were in the
// live transcript and not in the recording, nor in the refined transcript.
func TestFirstWordsDeepgramHearsAfterAReconnectAreAllInTheRecording(t *testing.T) {
	opensAPrerollBefore(t, outageRecording(t, 9, 12, 2), 12, 2)
}

// TestFirstWordsAfterALongOutageAreAllInTheRecording fails if words Deepgram
// hears after an outage that nearly filled the live client's 60 s buffer,
// with its first result some seconds after the reconnect, have lost their
// start: the recorder kept only the buffer's length plus 5 s, so the delay to
// Deepgram's first result came out of the 5 s before the words, and past 5 s
// out of the words.
func TestFirstWordsAfterALongOutageAreAllInTheRecording(t *testing.T) {
	opensAPrerollBefore(t, outageRecording(t, 9, 55, 12), 55, 12)
}
