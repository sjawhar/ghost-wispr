package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

func labelsString(labels []uint16) string {
	parts := make([]string, len(labels))
	for i, label := range labels {
		parts[i] = fmt.Sprint(label)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// TestFirstWordsDeepgramHearsAfterAReconnectAreAllInTheRecording fails if a
// session's recording starts after the first words its live transcript holds
// when Deepgram transcribes them late. While Deepgram is not connected, the
// live client buffers the microphone's audio (up to 60 s) and sends it once a
// socket connects again; Deepgram's results for it come only then. The
// recording kept only the last 5 s before those results came back, so the
// first words of a meeting that began during the outage were in the live
// transcript and not in the recording, nor in the refined transcript.
func TestFirstWordsDeepgramHearsAfterAReconnectAreAllInTheRecording(t *testing.T) {
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
	defer cancel()
	rc := newLiveTranscription(ctx, "test-key",
		&interfaces.ClientOptions{Host: host},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{SampleRate: 16000, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		manager, newWarningsRegistry(), nil)
	t.Cleanup(func() { _ = rc.Close() })
	t.Cleanup(func() { _ = manager.ForceEndSession(context.Background()) })
	mic := recorder.Writer(rc)
	say := func(labels ...uint16) {
		t.Helper()
		for _, label := range labels {
			if _, err := mic.Write(reconnectSecond(label)); err != nil {
				t.Fatalf("capture second %d: %v", label, err)
			}
		}
	}

	waitUntil(t, "the first socket", func() bool { return rc.IsConnected() && peer.latest() != nil })
	first := peer.latest()
	say(1, 2) // the room before anyone speaks
	waitUntil(t, "the first socket to get its audio", func() bool { return first.streamSeconds() >= 2 })

	// Deepgram drops the socket and refuses the next ones: the room's audio
	// waits in the live client.
	peer.refusing.Store(true)
	peer.dropAll()
	waitUntil(t, "the live client to see the socket gone", func() bool {
		say(3)
		return !rc.IsConnected()
	})
	say(10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21) // "Good morning, everyone. This is a synthetic test..."

	// Deepgram is back: the live client sends what it buffered to a new
	// socket, then the room's audio as it comes.
	peer.refusing.Store(false)
	waitUntil(t, "a second socket", func() bool { return peer.latest() != first })
	second := peer.latest()
	waitUntil(t, "the buffered audio to reach the second socket", func() bool {
		at, ok := second.heardAt(10)
		return ok && second.streamSeconds() >= at+12
	})
	say(22, 23)
	spoken, _ := second.heardAt(10)

	// Deepgram's results for the second socket's stream, timed in it.
	second.result(t, true, false, 0, spoken, 0)
	second.result(t, false, false, spoken, 2, spoken, "Good", "morning,")
	second.result(t, true, true, spoken, 12, spoken, "Good", "morning,", "everyone.", "This", "is", "a", "synthetic", "test", "of", "the", "office", "recorder.")
	waitUntil(t, "the first words to open a session", func() bool {
		id, _ := manager.ActiveSession()
		return id != ""
	})
	// The session is current before its recording starts: wait for the start.
	manager.WaitForSessionStarts()
	id, _ := manager.ActiveSession()

	got := recordedLabels(t, recording.RawPath(audioDir, id))
	utterance := []uint16{10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23}
	if len(got) < len(utterance) || !slices.Equal(got[len(got)-len(utterance):], utterance) {
		t.Fatalf("the recording holds seconds %s; want it to end with %s, from the first words the live transcript holds", labelsString(got), labelsString(utterance))
	}
}
