package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// namedWord is the first word of a result with the capture clocks read for
// it while its result was handed on: when it was captured, how far behind
// the capture it is, and all the audio captured then.
type namedWord struct {
	text                 string
	before, since, total time.Duration
}

// namedWordClock is a session manager that reads the capture clocks for the
// first word of each result, as the session manager does.
type namedWordClock struct {
	session.LifecycleManager
	rc    *transcribe.ResilientClient
	words chan namedWord
}

func (m *namedWordClock) Message(mr *api.MessageResponse) error {
	if len(mr.Channel.Alternatives) == 0 || len(mr.Channel.Alternatives[0].Words) == 0 {
		return nil
	}
	w := mr.Channel.Alternatives[0].Words[0]
	at := time.Duration(w.Start * float64(time.Second))
	m.words <- namedWord{w.Word, m.rc.CapturedBefore(at), m.rc.CapturedSince(at), m.rc.Captured()}
	return nil
}

// oneWordResult is a final result whose one word is at 10 ms into its
// socket's stream.
func oneWordResult(word string) []byte {
	return []byte(`{"type":"Results","is_final":true,"speech_final":true,"channel":{"alternatives":[{"transcript":"` + word + `.","words":[{"word":"` + word + `","punctuated_word":"` + word + `.","start":0.01,"end":0.02}]}]}}`)
}

// retireTrigger is what makes the resilient client retire its socket: an
// options change while it is connected, or the capture outrunning it while it
// drains.
type retireTrigger int

const (
	optionsChange retireTrigger = iota
	captureOutruns
)

func (tr retireTrigger) String() string {
	if tr == optionsChange {
		return "an options change replaces the socket"
	}
	return "the capture outruns the socket"
}

// retireRig is a resilient client on an 8192-byte buffer whose first
// Deepgram socket retires by trigger; 100 ms (3200 bytes) went to an earlier
// socket first, so the first socket's stream starts at capture byte 3200.
// After an options change the first socket has the whole 8192 bytes of audio
// and the next none; when the capture outruns it, the first socket's drain
// holds its first write, the first 4096 bytes, while 8192 more are written,
// and the next socket gets those, the kept tail. Either way the next socket's
// stream starts at capture byte 11392.
type retireRig struct {
	rc    *transcribe.ResilientClient
	dials atomic.Int32
	audio []byte
	// firstGot and nextGets are the audio the first and the next socket get
	// before the test writes more.
	firstGot, nextGets []byte
}

func startRetireRig(t *testing.T, srv *httptest.Server, callback transcriptCallback, trigger retireTrigger) *retireRig {
	t.Helper()
	rig := &retireRig{audio: make([]byte, 16384)}
	for i := range rig.audio {
		rig.audio[i] = byte(i*13 + i/127)
	}
	opts := &interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}
	firstHeld := make(chan *heldSocket, 1)
	factory := func(ctx context.Context) (io.WriteCloser, error) {
		s, err := dialDeepgramLive(ctx, "test-key", &interfaces.ClientOptions{Host: "ws://" + srv.Listener.Addr().String()}, opts, callback, time.Second)
		if err != nil {
			return nil, err
		}
		if rig.dials.Add(1) == 1 && trigger == captureOutruns {
			h := &heldSocket{deepgramSocket: s, entered: make(chan struct{}), release: make(chan struct{})}
			firstHeld <- h
			return h, nil
		}
		return s, nil
	}
	rig.rc = transcribe.NewResilientClient(context.Background(), factory, transcribe.ResilientConfig{BufferSize: 8192, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond}, nil, nil)
	t.Cleanup(func() { _ = rig.rc.Close() })
	rig.rc.Client = takingSocket{}
	_, _ = rig.rc.Write(make([]byte, 3200)) // 100 ms to an earlier socket
	rig.rc.Client = nil                     // that socket is gone: the next audio is buffered
	if trigger == optionsChange {
		rig.rc.ConnectInBackground()
		waitForHardwareWithin(t, 3*time.Second, "the first socket", func() bool { return rig.dials.Load() == 1 && rig.rc.IsConnected() })
		_, _ = rig.rc.Write(rig.audio[:8192]) // straight to the first socket
		rig.firstGot = rig.audio[:8192]
		rig.rc.OptionsChanged()
		return rig
	}
	_, _ = rig.rc.Write(rig.audio[:8192])
	rig.rc.ConnectInBackground()
	var first *heldSocket
	select {
	case first = <-firstHeld:
	case <-time.After(3 * time.Second):
		t.Fatal("the first socket was never dialled")
	}
	awaitClosedWithin(t, first.entered, "the drain never wrote to the first socket")
	_, _ = rig.rc.Write(rig.audio[8192:]) // the capture outruns the first socket
	close(first.release)                  // which takes its chunk after all
	rig.firstGot, rig.nextGets = rig.audio[:4096], rig.audio[8192:]
	return rig
}

func awaitClosedWithin(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal(what)
	}
}

// TestARetiredSocketsFinalIsKeptAndTimedInItsOwnStream fails if the final
// results Deepgram returns, after CloseStream, for the audio a replaced socket
// took are lost, as when that socket was closed at once; if a word in them is
// timed in the next socket's stream; or if it reaches the session after the
// next socket's word, though captured before. The socket's peer answers
// CloseStream only once the next socket's peer has sent its word, so the next
// socket must be dialled while the retired one waits, and its word must wait
// for the retired socket's. The retired socket's stream began at capture byte
// 3200, so its word at 10 ms was captured at 110 ms; the next socket's began
// at 11392, so its word at 10 ms at 366 ms.
func TestARetiredSocketsFinalIsKeptAndTimedInItsOwnStream(t *testing.T) {
	for _, trigger := range []retireTrigger{optionsChange, captureOutruns} {
		t.Run(trigger.String(), func(t *testing.T) { retiredSocketFinal(t, trigger) })
	}
}

func retiredSocketFinal(t *testing.T, trigger retireTrigger) {
	var handshakes atomic.Int32
	afterSent := make(chan struct{})
	firstPeerDone := make(chan struct{})
	marker := []byte("END-OF-AUDIO")
	received := make(chan []byte, 1)
	var mu sync.Mutex
	var firstGot []byte
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := handshakes.Add(1)
		if i == 1 {
			defer close(firstPeerDone)
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		var got []byte
		for {
			kind, p, err := c.ReadMessage()
			if err != nil {
				return
			}
			switch {
			case kind == websocket.BinaryMessage && i == 1:
				mu.Lock()
				firstGot = append(firstGot, p...)
				mu.Unlock()
			case kind == websocket.BinaryMessage && bytes.Equal(p, marker):
				received <- got
				_ = c.WriteMessage(websocket.TextMessage, oneWordResult("after"))
				close(afterSent)
			case kind == websocket.BinaryMessage:
				got = append(got, p...)
			case i == 1 && strings.Contains(string(p), "CloseStream"):
				select { // the next socket's peer has sent its word
				case <-afterSent:
				case <-time.After(3 * time.Second):
				}
				_ = c.WriteMessage(websocket.TextMessage, oneWordResult("before"))
				_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	clock := &namedWordClock{words: make(chan namedWord, 8)}
	rig := startRetireRig(t, srv, transcriptCallback{manager: clock}, trigger)
	clock.rc = rig.rc
	waitForHardwareWithin(t, 3*time.Second, "the next socket", func() bool { return rig.dials.Load() == 2 && rig.rc.IsConnected() })
	post := bytes.Repeat([]byte{0xA5, 0x73}, 1600)
	_, _ = rig.rc.Write(post)
	_, _ = rig.rc.Write(marker)
	wantNext := append(append([]byte(nil), rig.nextGets...), post...)
	select {
	case got := <-received:
		if !bytes.Equal(got, wantNext) {
			t.Fatalf("control: the next socket got %d bytes, want the newest %d, whole and in order", len(got), len(wantNext))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the next socket never got its audio")
	}

	// A copy more of either word would come within 100 ms of the last.
	var words []namedWord
	heard := map[string]bool{}
	deadline := time.After(3 * time.Second)
	var settled <-chan time.Time
collect:
	for {
		select {
		case w := <-clock.words:
			words = append(words, w)
			heard[w.text] = true
			if heard["before"] && heard["after"] {
				settled = time.After(100 * time.Millisecond)
			}
		case <-settled:
			break collect
		case <-deadline:
			break collect
		}
	}
	awaitClosedWithin(t, firstPeerDone, "the first socket's peer never got CloseStream")
	mu.Lock()
	prefix := append([]byte(nil), firstGot...)
	mu.Unlock()
	if !bytes.Equal(prefix, rig.firstGot) {
		t.Errorf("control: the first socket's peer got %d bytes, want the first %d captured after the earlier socket", len(prefix), len(rig.firstGot))
	}
	if len(words) != 2 || words[0].text != "before" || words[1].text != "after" {
		t.Errorf("the session manager heard %+v, want the first socket's word once, then the next socket's", words)
	}
	for _, w := range words {
		want := 366 * time.Millisecond
		if w.text == "before" {
			want = 110 * time.Millisecond
		}
		if w.before != want || w.since != w.total-want {
			t.Errorf("the word %q at 10ms maps to capture %v, %v behind, want %v and %v", w.text, w.before, w.since, want, w.total-want)
		}
	}
}

// TestARetiredSocketWhosePeerNeverAnswersDoesNotHoldUpItsSuccessor fails if
// the next socket, after the first was replaced, waits for the first socket's
// peer, which never answers CloseStream; or if the first socket stays open
// past its 1 s wait, so stalled sockets would pile up.
func TestARetiredSocketWhosePeerNeverAnswersDoesNotHoldUpItsSuccessor(t *testing.T) {
	for _, trigger := range []retireTrigger{optionsChange, captureOutruns} {
		t.Run(trigger.String(), func(t *testing.T) { retiredSocketNeverAnswered(t, trigger) })
	}
}

func retiredSocketNeverAnswered(t *testing.T, trigger retireTrigger) {
	var handshakes atomic.Int32
	var closeStreamAt atomic.Int64
	firstEnded := make(chan struct{})
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := handshakes.Add(1)
		if i == 1 {
			defer close(firstEnded)
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			kind, p, err := c.ReadMessage()
			if err != nil {
				return
			}
			if i == 1 && kind == websocket.TextMessage && strings.Contains(string(p), "CloseStream") {
				closeStreamAt.CompareAndSwap(0, time.Now().UnixNano())
			}
		}
	}))
	t.Cleanup(srv.Close)
	rig := startRetireRig(t, srv, transcriptCallback{}, trigger)
	waitForHardwareWithin(t, 3*time.Second, "the next socket", func() bool { return rig.dials.Load() == 2 && rig.rc.IsConnected() })
	connectedAt := time.Now()
	if isClosedChan(firstEnded) {
		t.Error("the next socket connected only after the first socket closed: it waited for a peer that never answers")
	}
	waitForHardwareWithin(t, 3*time.Second, "CloseStream at the first socket's peer", func() bool { return closeStreamAt.Load() != 0 })
	sent := time.Unix(0, closeStreamAt.Load())
	if waited := connectedAt.Sub(sent); waited > 500*time.Millisecond {
		t.Errorf("the next socket connected %v after the first ended its stream, want well within the first's 1s wait", waited)
	}
	awaitClosedWithin(t, firstEnded, "the first socket stayed open")
	if open := time.Since(sent); open < 900*time.Millisecond || open > 1500*time.Millisecond {
		t.Errorf("the first socket closed %v after it ended its stream, want at its 1s wait", open)
	}
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	waitForHardwareWithin(t, time.Second, "one socket to the peer", func() bool { return establishedTo(t, port) == 1 })
}

// TestAGracefulCloseEndsWithinItsWaitWhileAWriteIsStuck fails if a socket's
// graceful close waits past its wait for a write stuck on a peer that stopped
// reading, until that write's own 10 s limit: a retired socket can be on such
// a peer, and must not outlive its wait.
func TestAGracefulCloseEndsWithinItsWaitWhileAWriteIsStuck(t *testing.T) {
	peerDone := make(chan struct{})
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		<-peerDone // never reads
	}))
	t.Cleanup(func() {
		close(peerDone)
		srv.Close()
	})
	opts := &interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}
	s, err := dialDeepgramLive(context.Background(), "test-key", &interfaces.ClientOptions{Host: "ws://" + srv.Listener.Addr().String()}, opts, transcriptCallback{}, 10*time.Second)
	if err != nil {
		t.Fatalf("dial the local peer: %v", err)
	}
	var writing atomic.Int64 // when the write under way began, in Unix nanoseconds
	go func() {
		chunk := make([]byte, 1<<22)
		for {
			writing.Store(time.Now().UnixNano())
			if _, err := s.Write(chunk); err != nil {
				return
			}
		}
	}()
	waitForHardwareWithin(t, 10*time.Second, "a write stuck on the peer", func() bool {
		at := writing.Load()
		return at != 0 && time.Since(time.Unix(0, at)) > 300*time.Millisecond
	})
	start := time.Now()
	_ = s.CloseGracefully(300 * time.Millisecond)
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("the graceful close took %v with a write stuck on the peer, want about its 300ms wait", took)
	}
}

func isClosedChan(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
