package transcribe

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// streamedFake is a socket whose results are timed in the stream the client
// gives it (StreamedSocket). Asked to end its stream gracefully, it waits
// until the test lets its peer finish, as a peer returning its last results
// does, or until it is closed at once.
type streamedFake struct {
	optionsSocket
	stream    atomic.Pointer[Stream]
	wait      atomic.Int64  // the wait CloseGracefully was given
	ending    chan struct{} // closed when CloseGracefully starts
	finish    chan struct{} // closed by the test: the peer closed after its last results
	aborted   chan struct{} // closed when the socket is closed at once
	endOnce   sync.Once
	abortOnce sync.Once
}

func newStreamedFake(hold bool) *streamedFake {
	s := &streamedFake{ending: make(chan struct{}), finish: make(chan struct{}), aborted: make(chan struct{})}
	if hold {
		s.held, s.release = make(chan struct{}), make(chan struct{})
	}
	return s
}

func (s *streamedFake) UseStream(stream *Stream) { s.stream.Store(stream) }

func (s *streamedFake) CloseGracefully(wait time.Duration) error {
	s.wait.Store(int64(wait))
	s.close(true)
	s.endOnce.Do(func() { close(s.ending) })
	select {
	case <-s.finish:
	case <-s.aborted:
	}
	return nil
}

func (s *streamedFake) Close() error {
	s.close(false)
	s.abortOnce.Do(func() { close(s.aborted) })
	return nil
}

// streamOf returns the stream the client gave s.
func (s *streamedFake) streamOf(t *testing.T) *Stream {
	t.Helper()
	stream := s.stream.Load()
	if stream == nil {
		t.Fatal("the client gave the socket no stream")
	}
	return stream
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

// rotateThenNext connects first, the socket rc dials first, sends it 8192
// bytes, changes the options, and returns once next, the socket rc dials then,
// is connected while first still waits for its peer, with the 8192 bytes
// written after it.
func rotateThenNext(t *testing.T, rc *ResilientClient, first *streamedFake) []byte {
	t.Helper()
	audio := audioBytes(16384)
	rc.ConnectInBackground()
	waitConnected(t, rc)
	_, _ = rc.Write(audio[:8192]) // straight to the first socket
	rc.OptionsChanged()
	awaitClosed(t, first.ending, "the first socket never ended its stream gracefully")
	waitConnected(t, rc) // while the first socket's peer still owes its results
	_, _ = rc.Write(audio[8192:])
	return audio
}

// TestARetiredSocketsLastResultsAreTimedInItsOwnStream fails if a socket an
// options change replaced is closed at once, losing the final results
// Deepgram owes for the audio it took; if the next socket is dialled only
// after that socket's peer returned them, so a stalled peer would hold it up;
// if a word on the retired socket, handed on while the next one is live, is
// timed in the next socket's stream; or if the next socket's result is handed
// on before the retired socket has closed, ahead of words captured before it.
// The first socket's stream starts at capture byte 0 and the next one's at
// 8192 (256 ms): a word at 10 ms is at capture 10 ms on the first and 266 ms
// on the next.
func TestARetiredSocketsLastResultsAreTimedInItsOwnStream(t *testing.T) {
	first, next := newStreamedFake(false), newStreamedFake(false)
	f := &sequenceFactory{next: []io.WriteCloser{first, next}}
	rc := newTailClient(t, f)
	audio := rotateThenNext(t, rc, first)

	if isClosed(first.aborted) {
		t.Error("the first socket was closed at once, before its peer returned its last results")
	}
	if got, want := time.Duration(first.wait.Load()), retiredSocketWait; got != want {
		t.Errorf("the first socket was given %v to return its last results, want %v", got, want)
	}
	if got := next.got(); !bytes.Equal(got, audio[8192:]) {
		t.Errorf("the next socket got %d bytes, want the %d written after the change, whole and in order", len(got), len(audio)-8192)
	}
	ms := time.Millisecond
	total := 512 * ms
	check := func(name string, stream *Stream, start time.Duration) {
		t.Helper()
		before, since := stream.CapturedBefore(10*ms), stream.CapturedSince(10*ms)
		if want := start + 10*ms; before != want || since != total-want {
			t.Errorf("a word at 10ms on %s maps to capture %v, %v behind, want %v and %v", name, before, since, want, total-want)
		}
	}
	retired, current := first.streamOf(t), next.streamOf(t)
	if !retired.Admit() {
		t.Fatal("the retired socket's last result was refused within its wait")
	}
	check("the retired socket", retired, 0)
	nextAdmitted := make(chan bool, 1)
	go func() { nextAdmitted <- current.Admit() }()
	select {
	case <-nextAdmitted:
		t.Error("the next socket's result was admitted while the retired socket could still hand on its last")
	case <-time.After(50 * ms):
	}
	checkStreamClock(t, rc, len(audio), 8192)

	close(first.finish) // the peer returned its last results and closed
	select {
	case ok := <-nextAdmitted:
		if !ok {
			t.Error("the next socket's result was refused")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the next socket's result was never admitted after the retired socket closed")
	}
	check("the next socket", current, 256*ms)
	if n := retiredCount(rc); n != 0 {
		t.Errorf("the client still holds %d retired sockets after their close returned", n)
	}
}

// TestTheNextSocketsResultsWaitNoLongerThanTheRetiredSocketsWait fails if the
// next socket's results wait for a retired socket past retiredSocketWait from
// its retirement, as they did while its close waited for a result the session
// took slowly; or if a retired socket's result is still handed on after that,
// where it would come after the next socket's. The first socket's close never
// returns here, as when its reader is stuck in the session.
func TestTheNextSocketsResultsWaitNoLongerThanTheRetiredSocketsWait(t *testing.T) {
	first, next := newStreamedFake(false), newStreamedFake(false)
	f := &sequenceFactory{next: []io.WriteCloser{first, next}}
	rc := newTailClient(t, f)
	t.Cleanup(func() { _ = rc.Close() })
	rotateThenNext(t, rc, first)
	retiredAt := time.Now() // just after the first socket's close began
	retired, current := first.streamOf(t), next.streamOf(t)
	if !retired.Admit() {
		t.Fatal("control: the retired socket's result was refused within its wait")
	}

	nextAdmitted := make(chan time.Duration, 1)
	go func() {
		if current.Admit() {
			nextAdmitted <- time.Since(retiredAt)
		}
	}()
	select {
	case waited := <-nextAdmitted:
		if waited < retiredSocketWait-100*time.Millisecond {
			t.Errorf("the next socket's result was admitted %v after the retirement, before the retired socket's wait ran out", waited)
		}
	case <-time.After(retiredSocketWait + 400*time.Millisecond):
		t.Fatalf("the next socket's result was still held %v after the retirement: it waits for the retired socket past its wait", retiredSocketWait+400*time.Millisecond)
	}
	if retired.Admit() {
		t.Error("the retired socket's result was admitted after its wait ran out, after the next socket's")
	}
	if isClosed(first.aborted) {
		t.Error("control: the first socket was closed at once")
	}
}

// TestOneSocketRetiresAtATime fails if a socket still waiting for its peer's
// last results stays open when the next socket retires too, so that options
// changed again and again pile up sockets; or if Close leaves a retired
// socket open. The first socket's peer never answers; the second socket
// retires while it waits, and the first is closed at once then.
func TestOneSocketRetiresAtATime(t *testing.T) {
	first, second, third := newStreamedFake(false), newStreamedFake(false), newStreamedFake(false)
	f := &sequenceFactory{next: []io.WriteCloser{first, second, third}}
	rc := newTailClient(t, f)
	audio := audioBytes(24576)
	rc.ConnectInBackground()
	waitConnected(t, rc)
	_, _ = rc.Write(audio[:8192])
	rc.OptionsChanged()
	awaitClosed(t, first.ending, "the first socket never retired")
	waitConnected(t, rc)
	if isClosed(first.aborted) {
		t.Fatal("control: the first socket was closed at once before the second retired")
	}
	_, _ = rc.Write(audio[8192:16384])
	rc.OptionsChanged()
	awaitClosed(t, second.ending, "the second socket never retired")
	awaitClosed(t, first.aborted, "the first socket, still waiting for its peer, stayed open when the second retired")
	waitConnected(t, rc)
	_, _ = rc.Write(audio[16384:])

	if isClosed(second.aborted) {
		t.Error("the second socket was closed at once, before its peer returned its last results")
	}
	if got := third.got(); !bytes.Equal(got, audio[16384:]) {
		t.Errorf("the third socket got %d bytes, want the %d written after the second change, whole and in order", len(got), len(audio)-16384)
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
