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

// TestABufferOverflowDropsTheOldestAudioFirstWhileAChunkIsPending fails if
// the buffer, overflowing while the drain retries a chunk a socket refused,
// drops audio after that chunk and keeps the chunk: the next socket then gets
// the chunk, a gap, and the newest audio, and no single start of its stream
// maps both to the times they were captured (a word after the gap read 64 ms
// early). The oldest audio goes first, the pending chunk's before the
// buffer's, so what is kept is one tail of the capture, which the next socket
// gets whole, in order, its stream starting where that tail starts.
func TestABufferOverflowDropsTheOldestAudioFirstWhileAChunkIsPending(t *testing.T) {
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
	_, _ = rc.Write(audio[8192:]) // the buffer overflows while the chunk is pending
	close(failed.release)         // the first socket refuses the chunk
	waitConnected(t, rc)

	if got := failed.got(); len(got) != 0 {
		t.Fatalf("control: the first socket took %d bytes, want none", len(got))
	}
	if got := next.got(); !bytes.Equal(got, audio[8192:]) {
		at := 0
		for at < min(len(got), len(audio)-8192) && got[at] == audio[8192+at] {
			at++
		}
		t.Errorf("the next socket got %d bytes, want the newest %d of the capture, whole and in order, the tail an 8192-byte buffer keeps; they differ from byte %d", len(got), len(audio)-8192, at)
	}
	checkStreamClock(t, rc, len(audio), 8192)
	if n := f.dials(); n != 2 {
		t.Errorf("the factory dialled %d sockets, want 2", n)
	}
}

// TestAnOverflowPastTheChunkInFlightEndsThatSocketsStream fails if a socket
// that took the chunk the drain was writing, while the buffer overflowed past
// that chunk, goes on to take the newest audio: its stream would jump over the
// audio the overflow dropped, and the words after the jump would be read as
// captured earlier than they were. The socket ends its stream there, as one
// the capture outran by a whole buffer, gracefully, so Deepgram returns the
// results of the audio it took; the next socket's stream starts at the kept
// tail.
func TestAnOverflowPastTheChunkInFlightEndsThatSocketsStream(t *testing.T) {
	first := &optionsSocket{held: make(chan struct{}), release: make(chan struct{})}
	second := &optionsSocket{}
	f := &sequenceFactory{next: []io.WriteCloser{first, second}}
	rc := newTailClient(t, f)
	audio := audioBytes(16384)
	_, _ = rc.Write(audio[:8192])
	rc.ConnectInBackground()
	select {
	case <-first.held:
	case <-time.After(3 * time.Second):
		t.Fatal("the drain never wrote to the first socket")
	}
	_, _ = rc.Write(audio[8192:]) // drops the chunk in flight and the buffer's oldest half
	close(first.release)          // the first socket takes the chunk after all
	waitConnected(t, rc)

	if got := first.got(); !bytes.Equal(got, audio[:4096]) {
		t.Errorf("the first socket got %d bytes, want the 4096 of the chunk it took and nothing after the gap", len(got))
	}
	if closed, graceful := first.waitClosed(); !closed || !graceful {
		t.Errorf("the first socket closed %t, gracefully %t, want closed gracefully: Deepgram owes the results of the audio it took", closed, graceful)
	}
	if got := second.got(); !bytes.Equal(got, audio[8192:]) {
		t.Errorf("the second socket got %d bytes, want the kept tail, the newest %d, whole and in order", len(got), len(audio)-8192)
	}
	checkStreamClock(t, rc, len(audio), 8192)
	if n, most := f.dials(), f.counter.maxInDial.Load(); n != 2 || most != 1 {
		t.Errorf("the factory dialled %d sockets, at most %d at once, want 2 and 1", n, most)
	}
}

// TestAnOverflowThatTheDeliveredChunkCoversKeepsTheSocket fails if the
// buffer, overflowing into only the front of the chunk in flight, which the
// socket then takes whole, drops audio from behind the chunk or sends the
// chunk's bytes twice, or ends the socket: the socket took exactly the audio
// the overflow would have dropped, so its stream goes on with no gap.
func TestAnOverflowThatTheDeliveredChunkCoversKeepsTheSocket(t *testing.T) {
	only := &optionsSocket{held: make(chan struct{}), release: make(chan struct{})}
	f := &sequenceFactory{next: []io.WriteCloser{only}}
	rc := newTailClient(t, f)
	audio := audioBytes(10240)
	_, _ = rc.Write(audio[:8192])
	rc.ConnectInBackground()
	select {
	case <-only.held:
	case <-time.After(3 * time.Second):
		t.Fatal("the drain never wrote to the socket")
	}
	_, _ = rc.Write(audio[8192:]) // 2048 bytes over: the front of the chunk in flight
	close(only.release)           // the socket takes the whole chunk
	waitConnected(t, rc)

	if got := only.got(); !bytes.Equal(got, audio) {
		at := 0
		for at < min(len(got), len(audio)) && got[at] == audio[at] {
			at++
		}
		t.Errorf("the socket got %d bytes, want all %d, once and in order; they differ from byte %d", len(got), len(audio), at)
	}
	checkStreamClock(t, rc, len(audio), 0)
	if n := f.dials(); n != 1 {
		t.Errorf("the factory dialled %d sockets, want 1", n)
	}
}

// TestAPartialWriteAfterAnOverflowKeepsTheRestOfTheChunk fails if a socket
// that takes part of the chunk in flight, past the front an overflow dropped,
// and then fails, leaves the next socket anything but the rest of the chunk,
// then the buffer: the bytes it took are sent once, the ones the overflow
// dropped and it took are not dropped twice, and the next socket's stream
// starts where the first socket's ended.
func TestAPartialWriteAfterAnOverflowKeepsTheRestOfTheChunk(t *testing.T) {
	failed := &rejectingSocket{accept: 3000, held: make(chan struct{}), release: make(chan struct{})}
	next := &optionsSocket{}
	f := &sequenceFactory{next: []io.WriteCloser{failed, next}}
	rc := newTailClient(t, f)
	audio := audioBytes(10240)
	_, _ = rc.Write(audio[:8192])
	rc.ConnectInBackground()
	select {
	case <-failed.held:
	case <-time.After(3 * time.Second):
		t.Fatal("the drain never wrote to the first socket")
	}
	_, _ = rc.Write(audio[8192:]) // 2048 bytes over: the front of the chunk in flight
	close(failed.release)         // the socket takes 3000 bytes of the chunk, then fails
	waitConnected(t, rc)

	if got := failed.got(); !bytes.Equal(got, audio[:3000]) {
		t.Fatalf("control: the first socket took %d bytes, want the chunk's first 3000", len(got))
	}
	if got := next.got(); !bytes.Equal(got, audio[3000:]) {
		at := 0
		for at < min(len(got), len(audio)-3000) && got[at] == audio[3000+at] {
			at++
		}
		t.Errorf("the next socket got %d bytes, want the %d after what the first took, once and in order; they differ from byte %d", len(got), len(audio)-3000, at)
	}
	checkStreamClock(t, rc, len(audio), 3000)
}

// TestAnOverflowBeforeTheFirstChunkMovesTheStreamStart fails if the buffer,
// overflowing after a socket was dialled but before the drain wrote it any
// audio, leaves that socket's stream starting at audio the overflow dropped:
// its words would be read as captured that much earlier. Holding the drain's
// lock opens the moment between the dial and the first chunk.
func TestAnOverflowBeforeTheFirstChunkMovesTheStreamStart(t *testing.T) {
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

// TestAnOverflowBetweenChunksEndsTheSocketsStream fails if a socket that took
// a chunk goes on taking audio after an overflow, between that chunk and the
// next, dropped the audio it was to take next: its stream would jump over it.
// The next drain step ends the socket's stream there instead. The steps run
// one at a time here, so the overflow lands between them.
func TestAnOverflowBetweenChunksEndsTheSocketsStream(t *testing.T) {
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
	if more, err := rc.drainChunk(buf); more || !errors.Is(err, errStreamGap) {
		t.Errorf("the drain step after the overflow = (%t, %v), want (false, %v): the socket's stream would jump", more, err, errStreamGap)
	}
	if got := socket.got(); !bytes.Equal(got, audio[:4096]) {
		t.Errorf("the socket got %d bytes, want the first 4096 and nothing after the gap", len(got))
	}
}

// TestARefusedWriteOnAReplacedSocketDoesNotEndItsSuccessor fails if the audio
// a replaced socket refused, which goes back to the buffer while its
// successor drains, makes the drain end that healthy successor as if an
// overflow had broken its stream.
func TestARefusedWriteOnAReplacedSocketDoesNotEndItsSuccessor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := &optionsSocket{held: make(chan struct{}), release: make(chan struct{})}
	successor := &optionsSocket{held: make(chan struct{}), release: make(chan struct{})}
	f := &sequenceFactory{next: []io.WriteCloser{successor}}
	rc := NewResilientClient(ctx, f.dial, ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond}, nil, nil)
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
		t.Errorf("the successor closed %t after %d dials, want open after 1: the refused audio is not an overflow", closed, f.dials())
	}
	want := append(append(append([]byte(nil), audio[1001:8003]...), audio[:1001]...), audio[8003:]...)
	if got := successor.got(); !bytes.Equal(got, want) {
		t.Errorf("the successor got %d bytes, want all %d: the buffered audio, the refused write, then the rest", len(got), len(want))
	}
}
