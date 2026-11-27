package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// placedWord is a word the session manager kept, with when it was captured,
// read on the stream its result came with.
type placedWord struct {
	text   string
	before time.Duration
}

// placingManager is the session manager, which also reports each word it
// keeps and when it was captured.
type placingManager struct {
	*session.Manager
	rc    *transcribe.ResilientClient
	words chan placedWord
}

func (m *placingManager) MessageKeptOn(stream session.LiveStream, mr *api.MessageResponse) (*api.MessageResponse, error) {
	kept, err := m.Manager.MessageKeptOn(stream, mr)
	if stream == nil {
		stream = m.rc
	}
	if kept != nil {
		for _, w := range kept.Channel.Alternatives[0].Words {
			m.words <- placedWord{w.Word, stream.CapturedBefore(time.Duration(w.Start * float64(time.Second)))}
		}
	}
	return kept, err
}

// heldAfterWrite is a socket whose first write reaches the peer, then waits
// for release before it returns.
type heldAfterWrite struct {
	*deepgramSocket
	entered, release chan struct{}
	once             sync.Once
}

func (s *heldAfterWrite) Write(p []byte) (int, error) {
	n, err := s.deepgramSocket.Write(p)
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	return n, err
}

func finalResult(text string, at float64) []byte {
	return fmt.Appendf(nil, `{"type":"Results","is_final":true,"speech_final":true,"channel":{"alternatives":[{"transcript":%q,"words":[{"word":%q,"punctuated_word":%q,"start":%g,"end":%g}]}]}}`, text, text, text, at, at+0.01)
}

// TestAnOverflowDuringADrainKeepsTheSocketAndItsResults fails if the buffer,
// overflowing by a whole buffer while the drain's first write to a socket is
// under way, makes the client replace that socket: Deepgram owes it the result
// of the 4096 bytes it took, however late, and a replaced socket's late
// result is dropped. The socket goes on with the audio the buffer kept, and
// its stream skips the 4096 bytes the overflow dropped, as on main (#151).
// The peer returns "Before." for the bytes it took 100 ms, or 1.3 s, after
// the write returned, past a replaced socket's 1 s wait, then "After." for
// the audio after the hole: the session stores both, captured at 10 ms and at
// 266 ms. From the seventh review of #130, whose probe this is.
func TestAnOverflowDuringADrainKeepsTheSocketAndItsResults(t *testing.T) {
	runParallel(t)
	for _, delay := range []time.Duration{100 * time.Millisecond, 1300 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) { overflowKeepsTheSocket(t, delay) })
	}
}

func overflowKeepsTheSocket(t *testing.T, delay time.Duration) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "overflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
	placer := &placingManager{Manager: manager, words: make(chan placedWord, 8)}

	var handshakes atomic.Int32
	var mu sync.Mutex
	var wire []byte
	prefixTaken, beforeSent := make(chan struct{}), make(chan struct{})
	releaseWrite := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseWrite) })
	t.Cleanup(release)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handshakes.Add(1) > 1 {
			return // a second socket: the first was replaced
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		var sendMu sync.Mutex
		send := func(p []byte) {
			sendMu.Lock()
			defer sendMu.Unlock()
			_ = c.WriteMessage(websocket.TextMessage, p)
		}
		total := 0
		for {
			kind, p, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.TextMessage {
				var msg struct{ Type string }
				if json.Unmarshal(p, &msg) == nil && msg.Type == "CloseStream" {
					<-beforeSent // the result owed comes first
					return
				}
				continue
			}
			total += len(p)
			mu.Lock()
			wire = append(wire, p...)
			mu.Unlock()
			switch total {
			case 4096: // the first chunk, before the overflow
				close(prefixTaken)
				go func() {
					<-releaseWrite
					time.Sleep(delay)
					send(finalResult("Before.", 0.01))
					close(beforeSent)
				}()
			case 12288: // the audio the buffer kept, after the hole
				go func() {
					<-beforeSent
					send(finalResult("After.", 0.138))
				}()
			}
		}
	}))
	t.Cleanup(srv.Close)

	opts := &interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}
	heldEntered := make(chan struct{})
	var dials atomic.Int32
	factory := func(ctx context.Context) (io.WriteCloser, error) {
		s, err := dialDeepgramLive(ctx, "test-key", &interfaces.ClientOptions{Host: "ws://" + srv.Listener.Addr().String()}, opts, transcriptCallback{manager: placer}, time.Second)
		if err != nil {
			return nil, err
		}
		if dials.Add(1) == 1 {
			return &heldAfterWrite{deepgramSocket: s, entered: heldEntered, release: releaseWrite}, nil
		}
		return s, nil
	}
	rc := transcribe.NewResilientClient(context.Background(), factory, transcribe.ResilientConfig{BufferSize: 8192, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond}, nil, nil)
	t.Cleanup(func() { _ = rc.Close() })
	t.Cleanup(release) // before the client's close joins the readers
	placer.rc = rc
	manager.SetLiveStream(rc)

	audio := make([]byte, 16384)
	for i := range audio {
		audio[i] = byte(i*13 + i/127)
	}
	_, _ = rc.Write(audio[:8192])
	rc.ConnectInBackground()
	awaitClosedWithin(t, heldEntered, "the drain never wrote to the socket")
	awaitClosedWithin(t, prefixTaken, "the peer never took the first 4096 bytes")
	_, _ = rc.Write(audio[8192:]) // a whole buffer more: the buffer drops its oldest 4096
	release()

	var words []placedWord
	deadline := time.After(delay + 3*time.Second)
collect:
	for {
		select {
		case w := <-placer.words:
			words = append(words, w)
			if w.text == "After." {
				break collect
			}
		case <-deadline:
			break collect
		}
	}
	mu.Lock()
	got := append([]byte(nil), wire...)
	mu.Unlock()
	want := append(append([]byte(nil), audio[:4096]...), audio[8192:]...)
	if !bytes.Equal(got, want) {
		t.Errorf("the socket's peer got %d bytes, want the 4096 it took, then the %d the buffer kept, once and in order", len(got), len(audio)-8192)
	}
	if n := handshakes.Load(); n != 1 {
		t.Errorf("%d sockets were dialled, want 1: an overflow replaces no healthy socket", n)
	}
	if len(words) != 2 || words[0].text != "Before." || words[1].text != "After." {
		t.Fatalf("the session kept %+v, want Before. then After.: a result owed for audio the socket took was lost", words)
	}
	for _, w := range words {
		want := 10 * time.Millisecond
		if w.text == "After." {
			want = 266 * time.Millisecond
		}
		if w.before != want {
			t.Errorf("%s maps to capture %v, want %v", w.text, w.before, want)
		}
	}
	id, _ := manager.ActiveSession()
	if id == "" {
		t.Fatal("no session was stored")
	}
	segments, err := store.GetSegments(id)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, s := range segments {
		texts = append(texts, s.Text)
	}
	if stored := strings.Join(texts, " "); !strings.Contains(stored, "Before.") || !strings.Contains(stored, "After.") {
		t.Errorf("the session stored %q, want Before. and After.", stored)
	}
}
