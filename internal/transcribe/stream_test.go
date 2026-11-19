package transcribe

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const secondOfAudio = 16000 * bytesPerSample

// heldHandOff is a live client that waits at release once the send queue has
// handed it a chunk, before it counts the chunk.
type heldHandOff struct {
	*ResilientClient
	entered chan struct{}
	release chan struct{}
}

func (h *heldHandOff) writeFromQueue(p []byte, queued *atomic.Int64) (int, error) {
	h.entered <- struct{}{}
	<-h.release
	return h.ResilientClient.writeFromQueue(p, queued)
}

// TestCapturedCountsAChunkTheQueueHandsOn fails if Captured, the count a
// capture resume marks, leaves out a chunk the send queue has handed to the
// client that the client has not counted yet: a resume then marked the pause
// a chunk early, and the words of that chunk, from before the pause, reached
// the session the resume's words open. The wait widens a moment that is a
// function call in the app. TestCapturedSinceCountsTheAudioQueuedBeforeTheClient
// catches the other side, a chunk counted twice while the socket write stalls.
func TestCapturedCountsAChunkTheQueueHandsOn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := NewResilientClient(ctx, nil, ResilientConfig{SampleRate: 16000}, nil, nil)
	handOff := &heldHandOff{ResilientClient: rc, entered: make(chan struct{}), release: make(chan struct{})}
	sender := NewAsyncSender(ctx, handOff, 4, nil)
	if _, err := sender.Write(make([]byte, secondOfAudio)); err != nil {
		t.Fatal(err)
	}
	<-handOff.entered
	during := rc.Captured()
	close(handOff.release)
	deadline := time.Now().Add(5 * time.Second)
	for rc.Captured() != time.Second {
		if time.Now().After(deadline) {
			t.Fatalf("Captured() = %v after the hand-off, want 1s", rc.Captured())
		}
		time.Sleep(time.Millisecond)
	}
	if during != time.Second {
		t.Fatalf("Captured() = %v while the send queue hands a second of audio to the client, want 1s", during)
	}
}

// TestCapturedSinceCountsFromTheSocketsFirstByte fails if CapturedSince does
// not time Deepgram's results from the first byte the current socket was
// sent: the oldest audio buffered when it connected, not the first audio the
// client ever took.
func TestCapturedSinceCountsFromTheSocketsFirstByte(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := &mockWriter{}
	rc := NewResilientClient(ctx, func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
	rc.Mu.Lock()
	rc.State = StateReconnecting
	rc.Mu.Unlock()
	// 3 s the client took before this socket, of which the last 2 s wait
	// for it: the first second went to a socket that is gone.
	_, _ = rc.Write(make([]byte, secondOfAudio))
	rc.Mu.Lock()
	rc.Buffer.Reset()
	rc.Mu.Unlock()
	_, _ = rc.Write(make([]byte, 2*secondOfAudio))
	rc.reconnect(0)
	_, _ = rc.Write(make([]byte, secondOfAudio))
	// 1 s into the socket's stream is the second second it was sent; 2 s
	// were captured after that point.
	if got := rc.CapturedSince(time.Second); got != 2*time.Second {
		t.Fatalf("CapturedSince(1s) = %v, want 2s", got)
	}
	if got := rc.MaxBehind(); got != DefaultBufferSeconds*time.Second {
		t.Fatalf("MaxBehind() = %v, want %ds, the default buffer", got, DefaultBufferSeconds)
	}
}

// TestFinalizeTakesTheAudioItDropsOutOfTheStream fails if audio Finalize
// drops from the buffer while a new socket drains it still counts in that
// socket's stream: Deepgram never hears it, so the words it hears after are
// timed earlier in the stream than that audio's place among the captured
// audio.
func TestFinalizeTakesTheAudioItDropsOutOfTheStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := NewResilientClient(ctx, nil, ResilientConfig{}, nil, nil)
	// A socket was sent 1 s and is draining 2 s more, which the pause's
	// Finalize drops.
	rc.Mu.Lock()
	rc.State = StateDraining
	rc.accepted, rc.origin = 3*secondOfAudio, 0
	_, _ = rc.Buffer.Write(make([]byte, 2*secondOfAudio))
	rc.Mu.Unlock()
	if err := rc.Finalize(); err != nil {
		t.Fatal(err)
	}
	// The socket's next audio is captured after the resume.
	_, _ = rc.Write(make([]byte, secondOfAudio))
	if got := rc.CapturedSince(time.Second); got != time.Second {
		t.Fatalf("CapturedSince(1s) = %v, want 1s: the stream's second second is the audio after the resume", got)
	}
}

// heldSocket is a Deepgram socket whose writes wait until release closes,
// as on a congested uplink; entered gets a signal at each write.
type heldSocket struct {
	entered chan struct{}
	release chan struct{}
}

func (s *heldSocket) Write(p []byte) (int, error) {
	s.entered <- struct{}{}
	<-s.release
	return len(p), nil
}

func (*heldSocket) Close() error { return nil }

func (*heldSocket) Finalize() error { return nil }

// TestCapturedSinceCountsTheAudioQueuedBeforeTheClient fails if audio the
// send queue in front of the client (AsyncSender) holds, or dropped because
// it was full, is left out of CapturedSince: on a congested uplink every
// socket write still finishes inside its time limit, the queue fills to 16 s
// and then drops, and a recording started up to 11 s after the first words.
func TestCapturedSinceCountsTheAudioQueuedBeforeTheClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := &heldSocket{entered: make(chan struct{}, 8), release: make(chan struct{})}
	t.Cleanup(func() { close(socket.release) })
	rc := NewResilientClient(ctx, func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
	rc.reconnect(0)
	sender := NewAsyncSender(ctx, rc, 4, nil)
	_, _ = sender.Write(make([]byte, secondOfAudio)) // the socket write that waits
	<-socket.entered
	for range 5 { // 3 s queued, then 2 s dropped
		_, _ = sender.Write(make([]byte, secondOfAudio))
	}
	if got, want := rc.CapturedSince(0), 6*time.Second; got != want {
		t.Fatalf("CapturedSince(0) = %v, want %v: the second sent, 3 s queued and 2 s dropped", got, want)
	}
}

// gateSocket takes writes only once open is closed, reporting each.
type gateSocket struct {
	entered chan struct{}
	open    chan struct{}
	got     chan int
}

func (s *gateSocket) Write(p []byte) (int, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.open
	s.got <- len(p)
	return len(p), nil
}

func (*gateSocket) Close() error    { return nil }
func (*gateSocket) Finalize() error { return nil }

// TestADropBeforeAWordDoesNotCountForIt fails if audio the send queue dropped
// before a word counts in how long ago that word was captured: every word
// after a drop then read as captured that much earlier, until the next
// reconnect, so recordings led with that much more audio, and a word spoken
// after an ending session's recording closed was taken as spoken before.
func TestADropBeforeAWordDoesNotCountForIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := &gateSocket{entered: make(chan struct{}, 1), open: make(chan struct{}), got: make(chan int, 64)}
	rc := NewResilientClient(ctx, func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
	rc.reconnect(0)
	sender := NewAsyncSender(ctx, rc, 3, nil)
	_, _ = sender.Write(make([]byte, secondOfAudio)) // stream 0-1 s; this write waits on the uplink
	<-socket.entered
	_, _ = sender.Write(make([]byte, secondOfAudio)) // queued: stream 1-2 s
	_, _ = sender.Write(make([]byte, secondOfAudio)) // queued: stream 2-3 s
	_, _ = sender.Write(make([]byte, secondOfAudio)) // dropped
	_, _ = sender.Write(make([]byte, secondOfAudio)) // dropped
	close(socket.open)                               // the uplink recovers
	for range 3 {
		<-socket.got
	}
	for range 10 { // stream 3-13 s
		_, _ = sender.Write(make([]byte, secondOfAudio))
		<-socket.got
	}
	time.Sleep(50 * time.Millisecond) // the sender's last slot is back
	// A word 5 s into the stream was captured 7 s into the capture, after
	// the 2 s dropped; of the 15 s captured, 8 s came after it.
	if got, want := rc.CapturedSince(5*time.Second), 8*time.Second; got != want {
		t.Fatalf("CapturedSince(5s) = %v, want %v: the drop came before the word", got, want)
	}
	// A word 2 s into the stream came before the drop, which counts for it.
	if got, want := rc.CapturedSince(2*time.Second), 13*time.Second; got != want {
		t.Fatalf("CapturedSince(2s) = %v, want %v: the 2 s dropped came after the word", got, want)
	}
}

// eventSocket logs its Close as "close <name>".
type eventSocket struct {
	name     string
	log      *eventLog
	writeErr error
}

func (s *eventSocket) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return len(p), nil
}

func (s *eventSocket) Close() error {
	s.log.add("close " + s.name)
	return nil
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// TestAReconnectClosesTheFailedSocketFirst fails if a reconnect dials the
// next socket before it has closed the one that failed. The app's socket
// closes only once its reader has handed on the last result, so a result
// timed in the old socket's stream cannot be read against the new one's
// (CapturedSince), and the old socket holds no connection meanwhile.
func TestAReconnectClosesTheFailedSocketFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &eventLog{}
	first := &eventSocket{name: "first", log: log}
	dials := 0
	var mu sync.Mutex
	rc := NewResilientClient(ctx, func(context.Context) (io.WriteCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		log.add("dial")
		if dials == 1 {
			return first, nil
		}
		return &eventSocket{name: "second", log: log}, nil
	}, ResilientConfig{InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond}, nil, nil)
	rc.reconnect(0)
	first.writeErr = errors.New("the peer closed the socket")
	_, _ = rc.Write(make([]byte, 320))
	redialled := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return dials == 2
	}
	deadline := time.Now().Add(5 * time.Second)
	for !rc.IsConnected() || !redialled() {
		if time.Now().After(deadline) {
			t.Fatalf("no reconnect: events %v", log.get())
		}
		time.Sleep(time.Millisecond)
	}
	if got, want := log.get(), []string{"dial", "close first", "dial"}; !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v: the failed socket closed before the next dial", got, want)
	}
}
