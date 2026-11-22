package transcribe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// checkStreamClock fails t unless the current socket's stream starts at
// capture byte first of total captured, as the capture clocks read it at the
// client's 16 kHz: a word at stream time d was captured first bytes plus d
// into the capture, and is total bytes minus that behind it.
func checkStreamClock(t *testing.T, rc *ResilientClient, total, first int) {
	t.Helper()
	bytesFor := func(n int) time.Duration { return time.Duration(n) * time.Second / 32000 }
	if got, want := rc.Captured(), bytesFor(total); got != want {
		t.Errorf("Captured = %v, want %v for %d captured bytes", got, want, total)
	}
	for _, at := range []time.Duration{0, time.Millisecond, 25 * time.Millisecond, 128 * time.Millisecond} {
		if got, want := rc.CapturedBefore(at), bytesFor(first)+at; got != want {
			t.Errorf("a word at stream time %v maps to capture %v, want %v: the stream's first byte is capture byte %d", at, got, want, first)
		}
		if got, want := rc.CapturedSince(at), bytesFor(total-first)-at; got != want {
			t.Errorf("a word at stream time %v is %v behind capture, want %v", at, got, want)
		}
	}
}

// checkPoints fails t unless each point of the current socket's stream, in
// bytes, maps to the capture byte points gives it, of total captured: the
// stream skips the audio it was never sent (holes).
func checkPoints(t *testing.T, rc *ResilientClient, total int, points map[int]int) {
	t.Helper()
	bytesFor := func(n int) time.Duration { return time.Duration(n) * time.Second / 32000 }
	for at, want := range points {
		if got := rc.CapturedBefore(bytesFor(at)); got != bytesFor(want) {
			t.Errorf("stream byte %d maps to capture %v, want %v (capture byte %d)", at, got, bytesFor(want), want)
		}
		if got := rc.CapturedSince(bytesFor(at)); got != bytesFor(total-want) {
			t.Errorf("stream byte %d is %v behind capture, want %v", at, got, bytesFor(total-want))
		}
	}
}

// sequenceFactory dials the sockets it is given, in order, one at a time,
// and records them; with none left, a dial fails.
type sequenceFactory struct {
	mu      sync.Mutex
	next    []io.WriteCloser
	dialled []io.WriteCloser
	counter dialCounter
}

func (f *sequenceFactory) dial(context.Context) (io.WriteCloser, error) {
	f.counter.enter()
	defer f.counter.leave()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.next) == 0 {
		return nil, errors.New("the test has no more sockets")
	}
	s := f.next[0]
	f.next = f.next[1:]
	f.dialled = append(f.dialled, s)
	return s, nil
}

func (f *sequenceFactory) dials() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dialled)
}

// newTailClient is a client with an 8192-byte buffer, half a second at its
// 16 kHz, dialling f.
func newTailClient(t *testing.T, f *sequenceFactory) *ResilientClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewResilientClient(ctx, f.dial, ResilientConfig{
		BufferSize: 8192, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond,
	}, nil, nil)
}

// overflowHole is the stream of a socket that took the first 4096 bytes of
// 16384 before an 8192-byte buffer overflowed by 4096: it is sent capture
// bytes 0 to 4096, then 8192 to 16384, and a point past its 4096th byte lies
// 4096 bytes later in the capture.
var overflowHole = map[int]int{0: 0, 800: 800, 4094: 4094, 4096: 8192, 4416: 8512, 12286: 16382}

// TestAnOverflowDuringADrainWriteKeepsTheSocket fails if a socket that took
// the chunk the drain was writing, while the buffer overflowed behind that
// chunk, is replaced: it is healthy, and Deepgram owes it the results of the
// audio it took, however late they come. It goes on with the audio the
// buffer kept, and its stream skips the audio the overflow dropped, as main's
// does (#151): a word after the hole maps to when it was captured.
func TestAnOverflowDuringADrainWriteKeepsTheSocket(t *testing.T) {
	only := &optionsSocket{held: make(chan struct{}), release: make(chan struct{})}
	f := &sequenceFactory{next: []io.WriteCloser{only}}
	rc := newTailClient(t, f)
	audio := audioBytes(16384)
	_, _ = rc.Write(audio[:8192])
	rc.ConnectInBackground()
	select {
	case <-only.held:
	case <-time.After(3 * time.Second):
		t.Fatal("the drain never wrote to the socket")
	}
	_, _ = rc.Write(audio[8192:]) // 4096 over: the buffer's oldest, behind the chunk in flight
	close(only.release)           // the socket takes the chunk
	waitConnected(t, rc)

	want := append(append([]byte(nil), audio[:4096]...), audio[8192:]...)
	if got := only.got(); !bytes.Equal(got, want) {
		t.Errorf("the socket got %d bytes, want the chunk it took, then the %d the buffer kept, once and in order", len(got), len(audio)-8192)
	}
	only.mu.Lock()
	closed := only.closed
	only.mu.Unlock()
	if closed || f.dials() != 1 {
		t.Errorf("the socket closed %t after %d dials, want open after 1: an overflow ends no healthy socket", closed, f.dials())
	}
	checkPoints(t, rc, len(audio), overflowHole)
}

// TestAnOverflowBetweenChunksKeepsTheSocket fails if a socket that took a
// chunk, before an overflow dropped the audio it was to take next, is ended
// at its next chunk rather than sent the audio the buffer kept, with its
// stream skipping the hole. The drain's steps run one at a time here, so the
// overflow lands between them.
func TestAnOverflowBetweenChunksKeepsTheSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := NewResilientClient(ctx, nil, ResilientConfig{BufferSize: 8192}, nil, nil)
	socket := &optionsSocket{}
	rc.State = StateDraining
	rc.Client = socket
	audio := audioBytes(16384)
	_, _ = rc.Write(audio[:8192]) // buffered while draining; the stream starts at 0
	buf := make([]byte, 4096)
	if more, err := rc.drainChunk(buf); !more || err != nil {
		t.Fatalf("control: the first drain step = (%t, %v), want (true, nil)", more, err)
	}
	_, _ = rc.Write(audio[8192:]) // 4096 over: the 4096 the socket was to take next
	for {
		more, err := rc.drainChunk(buf)
		if err != nil {
			t.Fatalf("a drain step after the overflow = (%t, %v), want no error: the socket goes on past the hole", more, err)
		}
		if !more {
			break
		}
	}
	want := append(append([]byte(nil), audio[:4096]...), audio[8192:]...)
	if got := socket.got(); !bytes.Equal(got, want) {
		t.Errorf("the socket got %d bytes, want the first 4096, then the %d the buffer kept", len(got), len(audio)-8192)
	}
	checkPoints(t, rc, len(audio), overflowHole)
}

// TestAnOverflowWhileARefusedChunkWaitsIsAHoleAfterIt fails if the buffer,
// overflowing while a socket refuses the chunk the drain wrote it, drops the
// chunk, or leaves the next socket's stream reading the audio after the
// overflow as if the dropped audio had been sent: the next socket gets the
// chunk, then what the buffer kept, and its stream skips the hole between
// them, which the overflow made before that socket was dialled.
func TestAnOverflowWhileARefusedChunkWaitsIsAHoleAfterIt(t *testing.T) {
	failed := &rejectingSocket{accept: 0, held: make(chan struct{}), release: make(chan struct{})}
	next := &optionsSocket{}
	f := &sequenceFactory{next: []io.WriteCloser{failed, next}}
	rc := newTailClient(t, f)
	audio := audioBytes(16384)
	_, _ = rc.Write(audio[:8192]) // fills the buffer: no socket yet
	rc.ConnectInBackground()
	select {
	case <-failed.held: // the drain took the first 4096 bytes, and waits
	case <-time.After(3 * time.Second):
		t.Fatal("the drain never wrote to the first socket")
	}
	_, _ = rc.Write(audio[8192:]) // the buffer overflows behind the chunk
	close(failed.release)         // the first socket refuses the chunk
	waitConnected(t, rc)

	if got := failed.got(); len(got) != 0 {
		t.Fatalf("control: the first socket took %d bytes, want none", len(got))
	}
	want := append(append([]byte(nil), audio[:4096]...), audio[8192:]...)
	if got := next.got(); !bytes.Equal(got, want) {
		t.Errorf("the next socket got %d bytes, want the refused chunk, then the %d the buffer kept, once and in order", len(got), len(audio)-8192)
	}
	checkPoints(t, rc, len(audio), overflowHole)
	if n := f.dials(); n != 2 {
		t.Errorf("the factory dialled %d sockets, want 2", n)
	}
}

// TestAPartialWriteThenAnOverflowKeepsTheRestOfTheChunk fails if a socket
// that takes part of the chunk in flight and then fails, while the buffer
// overflowed behind the chunk, leaves the next socket anything but the rest of
// the chunk, then what the buffer kept: the bytes it took are sent once, and
// the next socket's stream starts where the first socket's ended and skips
// the hole after the chunk.
func TestAPartialWriteThenAnOverflowKeepsTheRestOfTheChunk(t *testing.T) {
	failed := &rejectingSocket{accept: 3000, held: make(chan struct{}), release: make(chan struct{})}
	next := &optionsSocket{}
	f := &sequenceFactory{next: []io.WriteCloser{failed, next}}
	rc := newTailClient(t, f)
	audio := audioBytes(16384)
	_, _ = rc.Write(audio[:8192])
	rc.ConnectInBackground()
	select {
	case <-failed.held:
	case <-time.After(3 * time.Second):
		t.Fatal("the drain never wrote to the first socket")
	}
	_, _ = rc.Write(audio[8192:]) // 4096 over: the buffer's oldest, behind the chunk
	close(failed.release)         // the socket takes 3000 bytes of the chunk, then fails
	waitConnected(t, rc)

	if got := failed.got(); !bytes.Equal(got, audio[:3000]) {
		t.Fatalf("control: the first socket took %d bytes, want the chunk's first 3000", len(got))
	}
	want := append(append([]byte(nil), audio[3000:4096]...), audio[8192:]...)
	if got := next.got(); !bytes.Equal(got, want) {
		t.Errorf("the next socket got %d bytes, want the chunk's last 1096, then the %d the buffer kept, once and in order", len(got), len(audio)-8192)
	}
	checkPoints(t, rc, len(audio), map[int]int{0: 3000, 1094: 4094, 1096: 8192, 9286: 16382})
}

// TestAnOverflowBeforeTheFirstChunkIsAHoleAtTheStreamsStart fails if the
// buffer, overflowing after a socket was dialled but before the drain wrote it
// any audio, leaves that socket's stream starting at audio the overflow
// dropped: its words would be read as captured that much earlier. Holding the
// drain's lock opens the moment between the dial and the first chunk.
func TestAnOverflowBeforeTheFirstChunkIsAHoleAtTheStreamsStart(t *testing.T) {
	only := &optionsSocket{}
	f := &sequenceFactory{next: []io.WriteCloser{only}}
	rc := newTailClient(t, f)
	audio := audioBytes(12288)
	_, _ = rc.Write(audio[:8192])
	rc.drainMu.Lock() // the drain waits before its first chunk
	rc.ConnectInBackground()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rc.Mu.Lock()
		draining := rc.State == StateDraining
		rc.Mu.Unlock()
		if draining {
			break
		}
		if time.Now().After(deadline) {
			rc.drainMu.Unlock()
			t.Fatal("the socket was never dialled")
		}
		time.Sleep(time.Millisecond)
	}
	_, _ = rc.Write(audio[8192:]) // 4096 over: the buffer's oldest
	rc.drainMu.Unlock()
	waitConnected(t, rc)

	if got := only.got(); !bytes.Equal(got, audio[4096:]) {
		t.Errorf("the socket got %d bytes, want the newest %d, whole and in order", len(got), len(audio)-4096)
	}
	checkStreamClock(t, rc, len(audio), 4096)
}

// TestARefusedWriteOnAReplacedSocketDoesNotEndItsSuccessor fails if the audio
// a replaced socket refused, which goes back to the buffer while its
// successor drains, is lost, or ends that healthy successor.
func TestARefusedWriteOnAReplacedSocketDoesNotEndItsSuccessor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := &optionsSocket{held: make(chan struct{}), release: make(chan struct{})}
	successor := &optionsSocket{held: make(chan struct{}), release: make(chan struct{})}
	f := &sequenceFactory{next: []io.WriteCloser{successor}}
	dialable := make(chan struct{}) // the successor's dial waits until the audio is buffered
	dial := func(ctx context.Context) (io.WriteCloser, error) {
		<-dialable
		return f.dial(ctx)
	}
	rc := NewResilientClient(ctx, dial, ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond}, nil, nil)
	rc.Client = old // connected
	audio := audioBytes(9003)
	wrote := make(chan struct{})
	go func() {
		_, _ = rc.Write(audio[:1001]) // to the old socket, held, then refused
		close(wrote)
	}()
	select {
	case <-old.held:
	case <-time.After(3 * time.Second):
		t.Fatal("the write never reached the old socket")
	}
	rc.OptionsChanged() // the old socket is replaced while the write is held
	_, _ = rc.Write(audio[1001:8003])
	close(dialable)
	select {
	case <-successor.held: // the successor drains, its first chunk held
	case <-time.After(3 * time.Second):
		t.Fatal("the successor never drained")
	}
	if closed, _ := old.waitClosed(); !closed {
		t.Fatal("the replaced socket was never closed")
	}
	close(old.release) // the old socket, closed meanwhile, refuses the write
	select {
	case <-wrote:
	case <-time.After(3 * time.Second):
		t.Fatal("the held write never returned")
	}
	close(successor.release)
	waitConnected(t, rc)
	_, _ = rc.Write(audio[8003:])

	successor.mu.Lock()
	closed := successor.closed
	successor.mu.Unlock()
	if closed || f.dials() != 1 {
		t.Errorf("the successor closed %t after %d dials, want open after 1", closed, f.dials())
	}
	want := append(append(append([]byte(nil), audio[1001:8003]...), audio[:1001]...), audio[8003:]...)
	if got := successor.got(); !bytes.Equal(got, want) {
		t.Errorf("the successor got %d bytes, want all %d: the buffered audio, the refused write, then the rest", len(got), len(want))
	}
}
