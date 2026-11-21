package transcribe

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// relayingSocket is a socket that hands its results on through the client's
// relay (ResultRelay). Asked to end its stream gracefully, it waits until the
// test lets its peer finish, as a peer returning its last results does, or
// until it is closed at once.
type relayingSocket struct {
	optionsSocket
	relay     atomic.Pointer[func(handOn func())]
	wait      atomic.Int64  // the wait CloseGracefully was given
	ending    chan struct{} // closed when CloseGracefully starts
	finish    chan struct{} // closed by the test: the peer closed after its last results
	aborted   chan struct{} // closed when the socket is closed at once
	endOnce   sync.Once
	abortOnce sync.Once
}

func newRelayingSocket(hold bool) *relayingSocket {
	s := &relayingSocket{ending: make(chan struct{}), finish: make(chan struct{}), aborted: make(chan struct{})}
	if hold {
		s.held, s.release = make(chan struct{}), make(chan struct{})
	}
	return s
}

func (s *relayingSocket) RelayResultsThrough(relay func(handOn func())) { s.relay.Store(&relay) }

func (s *relayingSocket) CloseGracefully(wait time.Duration) error {
	s.wait.Store(int64(wait))
	s.close(true)
	s.endOnce.Do(func() { close(s.ending) })
	select {
	case <-s.finish:
	case <-s.aborted:
	}
	return nil
}

func (s *relayingSocket) Close() error {
	s.close(false)
	s.abortOnce.Do(func() { close(s.aborted) })
	return nil
}

// result hands on one result of this socket, as its reader would, and reads
// the capture clocks for a word at stream time at while it is handed on.
func (s *relayingSocket) result(t *testing.T, rc *ResilientClient, at time.Duration) (before, since time.Duration) {
	t.Helper()
	relay := s.relay.Load()
	if relay == nil {
		t.Fatal("the client gave the socket no relay")
	}
	(*relay)(func() { before, since = rc.CapturedBefore(at), rc.CapturedSince(at) })
	return before, since
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal(what)
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func retiredCount(rc *ResilientClient) int {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	return len(rc.retired)
}

// TestARetiredSocketsLastResultsAreTimedInItsOwnStream fails if a socket the
// capture outran by a whole buffer is closed at once, losing the final
// results Deepgram owes for the audio it took; if the next socket is dialled
// only after that socket's peer returned them, so a stalled peer would hold
// it up; if a word on the retired socket, handed on while the next one is
// live, is timed in the next socket's stream; or if the next socket's result
// reaches the session before the retired socket has closed, ahead of words
// captured before it. The first socket's stream starts at capture byte 0 and
// the next one's at 8192 (256 ms): a word at 10 ms is at capture 10 ms on the
// first and 266 ms on the next.
func TestARetiredSocketsLastResultsAreTimedInItsOwnStream(t *testing.T) {
	first, next := newRelayingSocket(true), newRelayingSocket(false)
	f := &sequenceFactory{next: []io.WriteCloser{first, next}}
	rc := newTailClient(t, f)
	audio := audioBytes(16384)
	_, _ = rc.Write(audio[:8192])
	rc.ConnectInBackground()
	awaitClosed(t, first.held, "the drain never wrote to the first socket")
	_, _ = rc.Write(audio[8192:]) // overflows past the chunk in flight
	close(first.release)          // the first socket takes the chunk after all
	awaitClosed(t, first.ending, "the first socket never ended its stream gracefully")
	waitConnected(t, rc) // while the first socket's peer still owes its results

	if isClosed(first.aborted) {
		t.Error("the first socket was closed at once, before its peer returned its last results")
	}
	if got, want := time.Duration(first.wait.Load()), retiredSocketWait; got != want {
		t.Errorf("the first socket was given %v to return its last results, want %v", got, want)
	}
	if got := next.got(); !bytes.Equal(got, audio[8192:]) {
		t.Errorf("the next socket got %d bytes, want the kept tail, the newest %d, whole and in order", len(got), len(audio)-8192)
	}
	ms := time.Millisecond
	total := 512 * ms
	check := func(name string, before, since, start time.Duration) {
		t.Helper()
		if want := start + 10*ms; before != want || since != total-want {
			t.Errorf("a word at 10ms on %s maps to capture %v, %v behind, want %v and %v", name, before, since, want, total-want)
		}
	}
	before, since := first.result(t, rc, 10*ms) // the retired socket's last result
	check("the retired socket", before, since, 0)
	relay := next.relay.Load()
	if relay == nil {
		t.Fatal("the client gave the next socket no relay")
	}
	var nextBefore, nextSince time.Duration
	nextHandedOn := make(chan struct{})
	go func() {
		(*relay)(func() { nextBefore, nextSince = rc.CapturedBefore(10*ms), rc.CapturedSince(10*ms) })
		close(nextHandedOn)
	}()
	select {
	case <-nextHandedOn:
		t.Error("the next socket's result was handed on while the retired socket could still hand on its last")
	case <-time.After(50 * ms):
	}
	checkStreamClock(t, rc, len(audio), 8192)

	close(first.finish) // the peer returned its last results and closed
	awaitClosed(t, nextHandedOn, "the next socket's result was never handed on after the retired socket closed")
	check("the next socket", nextBefore, nextSince, 256*ms)
	if n := retiredCount(rc); n != 0 {
		t.Errorf("the client still holds %d retired sockets after their close returned", n)
	}
}

// TestOneSocketRetiresAtATime fails if a socket still waiting for its peer's
// last results stays open when the next socket retires too, so that a path
// the capture outruns again and again piles up sockets; or if Close leaves a
// retired socket open. The first socket's peer never answers; the second
// socket retires while it waits, and the first is closed at once then.
func TestOneSocketRetiresAtATime(t *testing.T) {
	first, second, third := newRelayingSocket(true), newRelayingSocket(true), newRelayingSocket(false)
	f := &sequenceFactory{next: []io.WriteCloser{first, second, third}}
	rc := newTailClient(t, f)
	audio := audioBytes(24576)
	_, _ = rc.Write(audio[:8192])
	rc.ConnectInBackground()
	awaitClosed(t, first.held, "the drain never wrote to the first socket")
	_, _ = rc.Write(audio[8192:16384])
	close(first.release)
	awaitClosed(t, first.ending, "the first socket never retired")
	awaitClosed(t, second.held, "the drain never wrote to the second socket")
	if isClosed(first.aborted) {
		t.Fatal("control: the first socket was closed at once before the second retired")
	}
	_, _ = rc.Write(audio[16384:])
	close(second.release)
	awaitClosed(t, second.ending, "the second socket never retired")
	awaitClosed(t, first.aborted, "the first socket, still waiting for its peer, stayed open when the second retired")
	waitConnected(t, rc)

	if isClosed(second.aborted) {
		t.Error("the second socket was closed at once, before its peer returned its last results")
	}
	if got := third.got(); !bytes.Equal(got, audio[16384:]) {
		t.Errorf("the third socket got %d bytes, want the kept tail, the newest %d, whole and in order", len(got), len(audio)-16384)
	}
	checkStreamClock(t, rc, len(audio), 16384)

	closed := make(chan struct{})
	go func() {
		_ = rc.Close()
		close(closed)
	}()
	awaitClosed(t, closed, "Close did not return")
	if !isClosed(second.aborted) {
		t.Error("Close returned with the retired second socket still open")
	}
}
