package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// wordClock is a session manager that reads, for the first word of each
// result, the capture clocks the session manager reads: when the word was
// captured, and how far behind the capture it is.
type wordClock struct {
	session.LifecycleManager
	rc    *transcribe.ResilientClient
	heard chan [2]time.Duration
}

func (m *wordClock) Message(mr *api.MessageResponse) error {
	if len(mr.Channel.Alternatives) == 0 || len(mr.Channel.Alternatives[0].Words) == 0 {
		return nil
	}
	start := time.Duration(mr.Channel.Alternatives[0].Words[0].Start * float64(time.Second))
	m.heard <- [2]time.Duration{m.rc.CapturedBefore(start), m.rc.CapturedSince(start)}
	return nil
}

type takingSocket struct{}

func (takingSocket) Write(p []byte) (int, error) { return len(p), nil }
func (takingSocket) Close() error                { return nil }

// TestAWordAfterRefusedDrainsMapsToItsCaptureTime fails if a word Deepgram
// returns on the socket that took the buffered audio, after two of the app's
// Deepgram sockets refused it, is read as captured at another time than it
// was. 100 ms went to an earlier socket first. Without an overflow, the third
// socket gets the whole buffer, and its word at 10 ms was captured at 110 ms.
// When the buffer overflows while the second socket retries the refused chunk,
// the oldest audio goes first, the chunk included: the third socket gets the
// kept tail, the newest 8192 bytes, and its word at 10 ms was captured 10 ms
// after that tail began, at 302 ms; with the chunk kept ahead of a gap it was
// read as 174 ms.
func TestAWordAfterRefusedDrainsMapsToItsCaptureTime(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		name := "no overflow"
		if overflow {
			name = "the buffer overflows during the retry"
		}
		t.Run(name, func(t *testing.T) { wordAfterRefusedDrains(t, overflow) })
	}
}

func wordAfterRefusedDrains(t *testing.T, overflow bool) {
	refuse := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var handshakes atomic.Int32
	marker := []byte("END-OF-DRAIN")
	received := make(chan []byte, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(handshakes.Add(1)) - 1
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		if i < 2 { // Deepgram ends the stream with an error
			<-refuse[i]
			_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"Error","err_code":"NET0000","err_msg":"the stream failed"}`))
			_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, ""))
			return
		}
		var got []byte
		for {
			kind, p, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage {
				continue
			}
			if bytes.Equal(p, marker) {
				received <- got
				_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"Results","is_final":true,"speech_final":true,"channel":{"alternatives":[{"transcript":"Retried.","words":[{"word":"retried","punctuated_word":"Retried.","start":0.01,"end":0.02}]}]}}`))
				continue
			}
			got = append(got, p...)
		}
	}))
	t.Cleanup(srv.Close)
	opts := &interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}
	held := make(chan *heldSocket, 2)
	var dials atomic.Int32
	clock := &wordClock{heard: make(chan [2]time.Duration, 1)}
	factory := func(ctx context.Context) (io.WriteCloser, error) {
		s, err := dialDeepgramLive(ctx, "test-key", &interfaces.ClientOptions{Host: "ws://" + srv.Listener.Addr().String()}, opts, transcriptCallback{manager: clock}, time.Second)
		if err != nil {
			return nil, err
		}
		if dials.Add(1) <= 2 {
			h := &heldSocket{deepgramSocket: s, entered: make(chan struct{}), release: make(chan struct{})}
			held <- h
			return h, nil
		}
		return s, nil
	}
	bufferSize := 1 << 16
	if overflow {
		bufferSize = 8192
	}
	rc := transcribe.NewResilientClient(context.Background(), factory, transcribe.ResilientConfig{BufferSize: bufferSize, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond}, nil, nil)
	clock.rc = rc
	t.Cleanup(func() { _ = rc.Close() })
	rc.Client = takingSocket{}
	_, _ = rc.Write(make([]byte, 3200)) // 100 ms to an earlier socket
	rc.OptionsChanged()
	audio := make([]byte, 6144)
	for i := range audio {
		audio[i] = byte(i*13 + i/127)
	}
	_, _ = rc.Write(audio) // buffered for the next socket
	want := append([]byte(nil), audio...)
	captured := 3200 + len(audio)
	first := 3200 // the capture byte the third socket's stream starts at
	await := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal(what)
		}
	}
	for i := range 2 {
		var s *heldSocket
		select {
		case s = <-held:
		case <-time.After(3 * time.Second):
			t.Fatal("no socket was dialled")
		}
		await(s.entered, "the drain never wrote to the socket")
		if overflow && i == 1 {
			later := bytes.Repeat([]byte{0xB3, 0xC9}, 4096)
			_, _ = rc.Write(later) // overflows the 8192-byte buffer
			captured += len(later)
			// The oldest audio goes first, the pending chunk included: what
			// is kept is the newest 8192 bytes.
			want = later
			first = captured - len(later)
		}
		close(refuse[i])
		await(s.done, "the socket never ended on Deepgram's error")
		close(s.release)
	}
	waitForHardwareWithin(t, 3*time.Second, "the third socket", rc.IsConnected)
	_, _ = rc.Write(marker)
	captured += len(marker)
	select {
	case got := <-received:
		if !bytes.Equal(got, want) {
			t.Fatalf("the third socket got %d bytes, want the %d kept, whole and in order", len(got), len(want))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the third socket never got the buffered audio")
	}
	select {
	case got := <-clock.heard:
		wordAt := 10 * time.Millisecond
		wantBefore := time.Duration(first)*time.Second/32000 + wordAt
		wantSince := time.Duration(captured)*time.Second/32000 - wantBefore
		if got[0] != wantBefore || got[1] != wantSince {
			t.Errorf("the word at stream time %v maps to capture %v, %v behind, want %v and %v", wordAt, got[0], got[1], wantBefore, wantSince)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the third socket never returned the word")
	}
	if n := handshakes.Load(); n != 3 {
		t.Errorf("%d handshakes, want 3", n)
	}
}
