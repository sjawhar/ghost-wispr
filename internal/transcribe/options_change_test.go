package transcribe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// optionsSocket is a socket in the options-change tests: it keeps the audio
// it was sent and the options it was dialled with, can hold its first write
// until release, and records how it was closed.
type optionsSocket struct {
	noDelay bool // the options it was dialled with

	held    chan struct{} // closed once its first write is under way, when holding
	release chan struct{} // its first write returns once closed, when holding
	once    sync.Once

	mu       sync.Mutex
	data     []byte
	writing  bool
	closed   bool
	graceful bool
	// closedWhileWriting says a close came while a write was under way: a
	// second owner closing the socket the first is draining into.
	closedWhileWriting bool
}

func (s *optionsSocket) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.writing = true
	s.mu.Unlock()
	if s.held != nil {
		s.once.Do(func() {
			close(s.held)
			<-s.release
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writing = false
	if s.closed {
		return 0, errors.New("the socket is closed")
	}
	s.data = append(s.data, p...)
	return len(p), nil
}

func (s *optionsSocket) close(graceful bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writing {
		s.closedWhileWriting = true
	}
	s.closed, s.graceful = true, graceful
}

func (s *optionsSocket) Close() error { s.close(false); return nil }

func (s *optionsSocket) CloseGracefully(time.Duration) error { s.close(true); return nil }

func (s *optionsSocket) got() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...)
}

// dialCounter counts a factory's dials and the most in progress at once.
type dialCounter struct {
	dials, inDial, maxInDial atomic.Int32
}

func (d *dialCounter) enter() {
	d.dials.Add(1)
	n := d.inDial.Add(1)
	for {
		m := d.maxInDial.Load()
		if n <= m || d.maxInDial.CompareAndSwap(m, n) {
			return
		}
	}
}

func (d *dialCounter) leave() { d.inDial.Add(-1) }

func audioBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func waitConnected(t *testing.T, rc *ResilientClient) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !rc.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("the client never connected")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAnOptionsChangeDuringADrainWaitsForItAndRedialsOnce fails if an options
// change while the owner drains the buffer into a new socket starts a second
// owner, which closes the socket being drained, dials alongside the first, and
// can lose a buffered chunk; or if the owner does not redial once with the new
// options after the drain; or if any audio is lost or sent twice. The socket
// left stale ends gracefully.
func TestAnOptionsChangeDuringADrainWaitsForItAndRedialsOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var counter dialCounter
	var (
		mu      sync.Mutex
		sockets []*optionsSocket
	)
	held, release := make(chan struct{}), make(chan struct{})
	factory := func(context.Context) (io.WriteCloser, error) {
		counter.enter()
		defer counter.leave()
		s := &optionsSocket{}
		mu.Lock()
		if len(sockets) == 0 {
			s.held, s.release = held, release // the first redial's drain is held
		}
		sockets = append(sockets, s)
		mu.Unlock()
		return s, nil
	}
	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize: 1 << 16, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond,
	}, nil, nil)
	rc.State = StateConnected
	rc.Client = &mockWriter{writeErr: errors.New("the socket died")}

	audio := audioBytes(3000)
	_, _ = rc.Write(audio[:1000]) // fails: buffered, and the owner redials
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("the redial never drained into its socket")
	}
	rc.OptionsChanged() // during the drain: it only marks the change
	_, _ = rc.Write(audio[1000:2000])
	mu.Lock()
	first := sockets[0]
	mu.Unlock()
	first.mu.Lock()
	closedEarly := first.closed
	first.mu.Unlock()
	close(release)
	waitConnected(t, rc)
	_, _ = rc.Write(audio[2000:])

	if closedEarly || first.closedWhileWriting {
		t.Errorf("the socket being drained was closed during its drain (closed %t, while writing %t): a second owner", closedEarly, first.closedWhileWriting)
	}
	if n := counter.maxInDial.Load(); n != 1 {
		t.Errorf("%d dials were in progress at once, want 1: one owner", n)
	}
	if n := counter.dials.Load(); n != 2 {
		t.Errorf("the factory dialled %d times, want 2: the redial, then once more for the new options", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if !first.closed || !first.graceful {
		t.Errorf("the socket the change made stale closed %t, gracefully %t, want both", first.closed, first.graceful)
	}
	var sent []byte
	for _, s := range sockets {
		sent = append(sent, s.got()...)
	}
	if !bytes.Equal(sent, audio) {
		t.Errorf("the sockets got %d bytes, want the %d written, each once and in order", len(sent), len(audio))
	}
}

// TestAnOptionsFlipDuringAHandshakeEndsOnTheLatestOptions fails if an
// off-on-off change of the options, the last flip landing while the redial's
// handshake is under way, leaves the client on the socket that dialled with
// the intermediate options, or starts a second owner, or loses audio. The
// healthy socket the first change replaced ends gracefully.
func TestAnOptionsFlipDuringAHandshakeEndsOnTheLatestOptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var counter dialCounter
	var noDelay atomic.Bool // the options the factory dials with now
	var (
		mu      sync.Mutex
		sockets []*optionsSocket
	)
	handshake, releaseHandshake := make(chan struct{}), make(chan struct{})
	factory := func(context.Context) (io.WriteCloser, error) {
		counter.enter()
		defer counter.leave()
		s := &optionsSocket{noDelay: noDelay.Load()}
		mu.Lock()
		first := len(sockets) == 0
		sockets = append(sockets, s)
		mu.Unlock()
		if first { // the redial's handshake is held
			close(handshake)
			<-releaseHandshake
		}
		return s, nil
	}
	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize: 1 << 16, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond,
	}, nil, nil)
	live := &optionsSocket{noDelay: false} // the healthy socket, dialled with the voice off
	rc.State = StateConnected
	rc.Client = live

	audio := audioBytes(3000)
	_, _ = rc.Write(audio[:1000]) // to the healthy socket
	noDelay.Store(true)
	rc.OptionsChanged() // on: the owner rotates the healthy socket
	_, _ = rc.Write(audio[1000:2000])
	select {
	case <-handshake:
	case <-time.After(3 * time.Second):
		t.Fatal("the rotation never dialled")
	}
	noDelay.Store(false)
	rc.OptionsChanged() // off again, during the handshake
	close(releaseHandshake)
	waitConnected(t, rc)
	_, _ = rc.Write(audio[2000:])

	if n := counter.maxInDial.Load(); n != 1 {
		t.Errorf("%d dials were in progress at once, want 1: one owner", n)
	}
	mu.Lock()
	defer mu.Unlock()
	rc.Mu.Lock()
	current, _ := rc.Client.(*optionsSocket)
	rc.Mu.Unlock()
	if current == nil || current.noDelay {
		t.Errorf("the client ended on a socket dialled with no_delay %t, want the latest options, no_delay false", current != nil && current.noDelay)
	}
	if !live.closed || !live.graceful {
		t.Errorf("the healthy socket the change replaced closed %t, gracefully %t, want both", live.closed, live.graceful)
	}
	if len(sockets[0].got()) != 0 {
		t.Errorf("the socket dialled with the intermediate options got %d bytes, want none", len(sockets[0].got()))
	}
	sent := live.got()
	for _, s := range sockets {
		sent = append(sent, s.got()...)
	}
	if !bytes.Equal(sent, audio) {
		t.Errorf("the sockets got %d bytes, want the %d written, each once and in order", len(sent), len(audio))
	}
}

// TestAWriteThatFailsOnAReplacedSocketGoesToItsSuccessor fails if a write that
// found the client connected, then failed on its socket after an options
// change had already replaced that socket, marks the client disconnected and
// starts a reconnect, which closes the healthy successor, or strands the
// chunk. The chunk goes to the successor, and every byte arrives once.
func TestAWriteThatFailsOnAReplacedSocketGoesToItsSuccessor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var counter dialCounter
	var (
		mu      sync.Mutex
		sockets []*optionsSocket
	)
	factory := func(context.Context) (io.WriteCloser, error) {
		counter.enter()
		defer counter.leave()
		s := &optionsSocket{}
		mu.Lock()
		sockets = append(sockets, s)
		mu.Unlock()
		return s, nil
	}
	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize: 1 << 16, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond,
	}, nil, nil)
	// The healthy socket holds its first write until release, then fails it
	// if it was closed meanwhile.
	live := &optionsSocket{held: make(chan struct{}), release: make(chan struct{})}
	rc.State = StateConnected
	rc.Client = live

	audio := audioBytes(2000)
	wrote := make(chan struct{})
	go func() {
		_, _ = rc.Write(audio[:1000]) // finds the client connected, then is held
		close(wrote)
	}()
	select {
	case <-live.held:
	case <-time.After(3 * time.Second):
		t.Fatal("the write never reached the socket")
	}
	rc.OptionsChanged() // the healthy socket is replaced while the write is held
	waitConnected(t, rc)
	close(live.release) // the held write now fails: its socket is closed
	select {
	case <-wrote:
	case <-time.After(3 * time.Second):
		t.Fatal("the held write never returned")
	}
	_, _ = rc.Write(audio[1000:])
	time.Sleep(20 * time.Millisecond) // a wrong reconnect would dial by now

	mu.Lock()
	defer mu.Unlock()
	if len(sockets) != 1 {
		t.Fatalf("the factory dialled %d sockets, want 1: the rotation's; a write that failed on the replaced socket started a reconnect", len(sockets))
	}
	next := sockets[0]
	next.mu.Lock()
	closed := next.closed
	next.mu.Unlock()
	if closed || !rc.IsConnected() {
		t.Errorf("the successor closed %t, the client connected %t, want false and true", closed, rc.IsConnected())
	}
	if got := append(live.got(), next.got()...); !bytes.Equal(got, audio) {
		t.Errorf("the sockets got %d bytes, want the %d written, each once and in order", len(got), len(audio))
	}
}
