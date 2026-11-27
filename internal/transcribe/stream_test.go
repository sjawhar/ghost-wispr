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
	select {
	case <-handOff.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the send queue did not hand the chunk to the client with writeFromQueue, which counts it as it leaves the queue")
	}
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
	// Twice the default buffer and a drain chunk (128 ms at 16 kHz): the
	// session keeps recording history this far back.
	if got, want := rc.MaxBehind(), 2*DefaultBufferSeconds*time.Second+128*time.Millisecond; got != want {
		t.Fatalf("MaxBehind() = %v, want %v, twice the default buffer and a drain chunk", got, want)
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

// TestManyDropsKeepEachPointsCount fails if, past many drops of the send
// queue, a point of the stream between two of them stops counting the drop
// before it: the oldest two used to merge at the later place, so a word
// between them was timed by audio captured earlier than its own (Sol's
// review of #133 at d125fc1).
func TestManyDropsKeepEachPointsCount(t *testing.T) {
	rc := NewResilientClient(t.Context(), nil, ResilientConfig{SampleRate: 16000}, nil, nil)
	for range 1100 {
		_, _ = rc.Write(make([]byte, 32)) // 1 ms taken
		rc.sendQueueDropped(320)          // then 10 ms dropped
	}
	// 1.5 ms into the stream lies between the first drop, after the first
	// 1 ms, and the second, after the second: it counts the first.
	if got, want := rc.CapturedBefore(1500*time.Microsecond), 11500*time.Microsecond; got != want {
		t.Fatalf("CapturedBefore(1.5ms) = %v, want %v: the 1.5 ms of the stream and the 10 ms dropped before it", got, want)
	}
}

// TestCapturedAtTimesAPointByWhenItsAudioWasCaptured fails if CapturedAt
// does not place a point of the socket's stream at the time its audio was
// captured, on the system clock: the time of the buffer that holds it, less
// the buffer's audio after the point, whether the client took that buffer, a
// send queue in front of it holds it while the uplink stalls, or the queue
// dropped later ones. It must not move with when the time is asked, as when
// a result arrives late. The voice tells the words of the question it
// answers from words said over it by this time.
func TestCapturedAtTimesAPointByWhenItsAudioWasCaptured(t *testing.T) {
	// within fails unless got lies in [lo, hi].
	within := func(t *testing.T, what string, got time.Time, ok bool, lo, hi time.Time) {
		t.Helper()
		if !ok || got.Before(lo) || got.After(hi) {
			t.Fatalf("CapturedAt(%s) = %v, %v; want a time from %v to %v", what, got.Format(time.StampMicro), ok, lo.Format(time.StampMicro), hi.Format(time.StampMicro))
		}
	}
	t.Run("before any audio", func(t *testing.T) {
		rc := NewResilientClient(t.Context(), nil, ResilientConfig{}, nil, nil)
		if at, _, ok := rc.CapturedAt(0); ok {
			t.Fatalf("CapturedAt(0) = %v with no audio captured, want none", at)
		}
	})
	t.Run("the client's own writes", func(t *testing.T) {
		socket := &mockWriter{}
		rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
		rc.reconnect(0)
		lo1 := time.Now()
		_, _ = rc.Write(make([]byte, secondOfAudio)) // stream 0-1 s, captured by this write
		hi1 := time.Now()
		time.Sleep(100 * time.Millisecond)
		lo2 := time.Now()
		_, _ = rc.Write(make([]byte, secondOfAudio)) // stream 1-2 s
		hi2 := time.Now()
		time.Sleep(50 * time.Millisecond) // a result that arrives later
		at, _, ok := rc.CapturedAt(1500 * time.Millisecond)
		within(t, "1.5s", at, ok, lo2.Add(-500*time.Millisecond), hi2.Add(-500*time.Millisecond))
		at, _, ok = rc.CapturedAt(500 * time.Millisecond)
		within(t, "0.5s", at, ok, lo1.Add(-500*time.Millisecond), hi1.Add(-500*time.Millisecond))
		if at, _, ok := rc.CapturedAt(2500 * time.Millisecond); ok {
			t.Fatalf("CapturedAt(2.5s) = %v, after all the audio captured; want none", at)
		}
	})
	t.Run("a send queue on a stalled uplink", func(t *testing.T) {
		socket := &heldSocket{entered: make(chan struct{}, 8), release: make(chan struct{})}
		t.Cleanup(func() { close(socket.release) })
		rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
		rc.reconnect(0)
		sender := NewAsyncSender(t.Context(), rc, 4, nil)
		_, _ = sender.Write(make([]byte, secondOfAudio)) // stream 0-1 s: the socket write that waits
		<-socket.entered
		_, _ = sender.Write(make([]byte, secondOfAudio)) // stream 1-2 s, queued
		_, _ = sender.Write(make([]byte, secondOfAudio)) // stream 2-3 s, queued
		lo := time.Now()
		_, _ = sender.Write(make([]byte, secondOfAudio)) // stream 3-4 s, queued
		hi := time.Now()
		for range 2 {
			_, _ = sender.Write(make([]byte, secondOfAudio)) // dropped
		}
		time.Sleep(50 * time.Millisecond)
		// 3.5 s into the stream, half a second before its buffer's end.
		at, _, ok := rc.CapturedAt(3500 * time.Millisecond)
		within(t, "3.5s", at, ok, lo.Add(-500*time.Millisecond), hi.Add(-500*time.Millisecond))
	})
	t.Run("a word after the send queue dropped audio", func(t *testing.T) {
		// The queue's drops are in the count of audio captured but not in
		// the socket's stream: a word after them is in a later buffer than
		// its place in the stream reads (Sol's review of #133 at 0dfeaad
		// found the earlier cases blind to the drops, being before them).
		socket := &heldSocket{entered: make(chan struct{}, 8), release: make(chan struct{})}
		released := false
		t.Cleanup(func() {
			if !released {
				close(socket.release)
			}
		})
		rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
		rc.reconnect(0)
		sender := NewAsyncSender(t.Context(), rc, 4, nil)
		_, _ = sender.Write(make([]byte, secondOfAudio)) // stream 0-1 s: the socket write that waits
		<-socket.entered
		for range 3 {
			_, _ = sender.Write(make([]byte, secondOfAudio)) // stream 1-4 s, queued
		}
		for range 2 {
			_, _ = sender.Write(make([]byte, secondOfAudio)) // captured 4-6 s, dropped
		}
		close(socket.release)
		released = true
		for range 3 {
			<-socket.entered // the queued audio goes out
		}
		time.Sleep(100 * time.Millisecond)
		lo := time.Now()
		_, _ = sender.Write(make([]byte, secondOfAudio)) // captured 6-7 s, stream 4-5 s
		hi := time.Now()
		<-socket.entered
		at, _, ok := rc.CapturedAt(4500 * time.Millisecond)
		within(t, "4.5s", at, ok, lo.Add(-500*time.Millisecond), hi.Add(-500*time.Millisecond))
	})
	t.Run("a later buffer held back", func(t *testing.T) {
		// A buffer can reach the client late, as when the read of the light
		// before it is slow: its time is no capture time, and must not move
		// the words of the buffers before it.
		socket := &mockWriter{}
		rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
		rc.reconnect(0)
		sender := NewAsyncSender(t.Context(), rc, 4, nil)
		const quarter = secondOfAudio / 4
		lo := time.Now()
		_, _ = sender.Write(make([]byte, quarter)) // stream 0-0.25 s, on time
		hi := time.Now()
		time.Sleep(time.Second)
		_, _ = sender.Write(make([]byte, quarter)) // stream 0.25-0.5 s, a second late
		at, _, ok := rc.CapturedAt(0)
		within(t, "0s", at, ok, lo.Add(-250*time.Millisecond), hi.Add(-250*time.Millisecond))
	})
	t.Run("the first words after a gap in capture", func(t *testing.T) {
		// A capture pause leaves a gap in time that the count of audio does
		// not hold: a word just after it was captured with the buffer after
		// the gap, never at the time of the buffer before.
		socket := &mockWriter{}
		rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
		rc.reconnect(0)
		const quarter = secondOfAudio / 4
		_, _ = rc.Write(make([]byte, quarter)) // stream 0-0.25 s
		time.Sleep(time.Second)                // capture paused
		lo := time.Now()
		_, _ = rc.Write(make([]byte, quarter)) // stream 0.25-0.5 s, after the resume
		hi := time.Now()
		at, _, ok := rc.CapturedAt(250 * time.Millisecond)
		within(t, "0.25s", at, ok, lo.Add(-250*time.Millisecond), hi.Add(-250*time.Millisecond))
	})
	t.Run("a buffer held back after its capture", func(t *testing.T) {
		// The microphone's read time, marked before the light check, is the
		// buffer's: the words in it keep it however long the check held it.
		socket := &mockWriter{}
		rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
		rc.reconnect(0)
		sender := NewAsyncSender(t.Context(), rc, 4, nil)
		const quarter = secondOfAudio / 4
		read := time.Now()
		sender.MarkRead(read)
		time.Sleep(time.Second) // a slow read of the light
		_, _ = sender.Write(make([]byte, quarter))
		at, _, ok := rc.CapturedAt(125 * time.Millisecond) // halfway through it
		within(t, "0.125s", at, ok, read.Add(-125*time.Millisecond), read.Add(-125*time.Millisecond))
	})
	t.Run("audio a full buffer evicted while a new socket drains it", func(t *testing.T) {
		// The stream skips audio the buffer lost before it was sent: the
		// words after it are the audio written after, not the audio lost.
		rc := NewResilientClient(t.Context(), nil, ResilientConfig{SampleRate: 16000, BufferSize: 8192}, nil, nil)
		_, _ = rc.Write(make([]byte, 8192)) // A, 256 ms, buffered with no socket
		time.Sleep(300 * time.Millisecond)
		rc.Mu.Lock()
		rc.origin = rc.accepted - int64(rc.Buffer.Len()) // a socket connects and starts on A
		rc.State = StateDraining
		rc.Buffer.Discard(4096) // and has taken A's first half
		rc.Mu.Unlock()
		lo := time.Now()
		_, _ = rc.Write(make([]byte, 8192)) // B, buffered behind it: the full buffer evicts A's second half
		hi := time.Now()
		// The socket's 128 ms is B's start, 256 ms before B was captured.
		at, _, ok := rc.CapturedAt(128 * time.Millisecond)
		within(t, "0.128s", at, ok, lo.Add(-256*time.Millisecond), hi.Add(-256*time.Millisecond))
	})
	t.Run("a Finalize after some audio was sent", func(t *testing.T) {
		// Finalize drops the buffered audio, which comes after the audio
		// already sent: the words of that audio keep their times.
		socket := &mockWriter{}
		rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
		rc.reconnect(0)
		_, _ = rc.Write(make([]byte, secondOfAudio)) // A, sent
		rc.Mu.Lock()
		rc.State = StateDraining
		_, _ = rc.Buffer.Write(make([]byte, secondOfAudio)) // B, buffered, not yet sent
		rc.accepted += secondOfAudio
		rc.Mu.Unlock()
		before, _, ok := rc.CapturedAt(500 * time.Millisecond)
		if err := rc.Finalize(); err != nil {
			t.Fatal(err)
		}
		after, _, ok2 := rc.CapturedAt(500 * time.Millisecond)
		if !ok || !ok2 || !after.Equal(before) {
			t.Fatalf("a word in A captured at %v before Finalize and %v after, want the same", before.Format(time.StampMicro), after.Format(time.StampMicro))
		}
	})
}

// TestAChunkLargerThanTheBufferIsAHoleToo fails if, while a socket drains, a
// microphone chunk larger than the whole buffer loses its own first bytes to
// the buffer with no hole recorded: the next socket byte was then timed by
// audio captured 122 ms earlier than its own (Sol's review of #133 at
// 55bdf53).
func TestAChunkLargerThanTheBufferIsAHoleToo(t *testing.T) {
	socket := &mockWriter{}
	rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{SampleRate: 16000, BufferSize: 4096}, nil, nil)
	rc.reconnect(0)
	_, _ = rc.Write(make([]byte, 4096)) // sent: the socket's first 128 ms
	rc.Mu.Lock()
	rc.State = StateDraining
	rc.Mu.Unlock()
	_, _ = rc.Write(make([]byte, 8000)) // buffered: its first 3,904 bytes do not fit
	if got, want := rc.CapturedBefore(128*time.Millisecond), 250*time.Millisecond; got != want {
		t.Fatalf("CapturedBefore(128ms) = %v, want %v: the socket's next byte is the last 4,096 bytes of the 8,000-byte chunk", got, want)
	}
}

// TestAnOldPointKeepsItsTime fails if a point of the stream that the clock
// still times moves when later drops come: compacting old drop records into
// the counts every point shares moved CapturedAt(500ms) by the dropped
// second, though its stamp was still kept (Sol's review of #133 at 55bdf53).
func TestAnOldPointKeepsItsTime(t *testing.T) {
	socket := &mockWriter{}
	rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{SampleRate: 16000}, nil, nil)
	rc.reconnect(0)
	_, _ = rc.Write(make([]byte, secondOfAudio))
	rc.sendQueueDropped(secondOfAudio)
	for range 361 * 4 {
		_, _ = rc.Write(make([]byte, secondOfAudio/4))
	}
	before, _, ok := rc.CapturedAt(500 * time.Millisecond)
	if !ok {
		t.Fatal("control: the clock no longer times 0.5 s")
	}
	rc.sendQueueDropped(secondOfAudio / 4)
	if after, _, ok := rc.CapturedAt(500 * time.Millisecond); !ok || !after.Equal(before) {
		t.Fatalf("CapturedAt(500ms) = %v (%v) after a later drop, %v before; want it unchanged", after.Format(time.StampMicro), ok, before.Format(time.StampMicro))
	}
}

// TestAWordOlderThanTheClocksHistoryIsTimedByItsOldestAudio fails if a point
// of the stream older than every buffer the clock still times is untimed:
// the voice then takes its word as said when its result arrived, so a
// question whose result comes more than captureStamps buffers late stops
// its own answer (Sol's review of #133 at e6484db). It was captured before
// the oldest audio kept, so it is timed by that audio, the latest it can
// have been, and that time is not exact: every point that old has it, so it
// names none of them (Sol's twenty-ninth review of #191). The control: a
// point inside the oldest buffer kept is timed exactly, by its place in that
// buffer.
func TestAWordOlderThanTheClocksHistoryIsTimedByItsOldestAudio(t *testing.T) {
	socket := &mockWriter{}
	rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{SampleRate: 16000}, nil, nil)
	rc.reconnect(0)
	const quarter = secondOfAudio / 4
	for range captureStamps + 8 { // the first 8 quarter seconds are no longer kept
		_, _ = rc.Write(make([]byte, quarter))
	}
	oldest, exact, ok := rc.CapturedAt(2 * time.Second)
	if !ok || !exact {
		t.Fatalf("control: the clock no longer times the oldest buffer it keeps exactly (exact %v, ok %v)", exact, ok)
	}
	if at, exact, ok := rc.CapturedAt(2125 * time.Millisecond); !ok || !exact || !at.Equal(oldest.Add(125*time.Millisecond)) {
		t.Fatalf("control: CapturedAt(2.125s) = %v (exact %v, ok %v), want %v exactly, 125 ms into the oldest buffer kept", at.Format(time.StampMicro), exact, ok, oldest.Add(125*time.Millisecond).Format(time.StampMicro))
	}
	for _, point := range []time.Duration{0, 1900 * time.Millisecond} {
		if at, exact, ok := rc.CapturedAt(point); !ok || exact || !at.Equal(oldest) {
			t.Errorf("CapturedAt(%v) = %v (exact %v, ok %v), older than the clock's history; want %v, when its oldest audio was captured, not exact", point, at.Format(time.StampMicro), exact, ok, oldest.Format(time.StampMicro))
		}
	}
}

// stampOrder is a live client that records, as the send queue stamps a
// chunk, whether the chunk is already on the queue for the client to take:
// no client goroutine runs, so the check needs no timing.
type stampOrder struct {
	s             *AsyncSender
	stamps        int
	queuedAtStamp int
}

func (*stampOrder) watchSendQueue(func() int64) {}
func (*stampOrder) sendQueueDropped(int64)      {}

func (*stampOrder) writeFromQueue([]byte, *atomic.Int64) (int, error) {
	panic("no client takes from the queue in this test")
}

func (o *stampOrder) captured(time.Time, int64) {
	o.stamps++
	o.queuedAtStamp += len(o.s.queue)
}

// TestTheSendQueueStampsAChunkBeforeItHandsItOn fails if the send queue
// hands a chunk to the live client before it has stamped when the chunk was
// captured: a result for the chunk's words could then be timed from the
// stamp before it, which a capture pause can place long before. It reads
// the queue from inside the stamp, with no client goroutine, so it fails
// whenever the hand-off comes first, however a machine schedules it (Sol's
// review of #133 at aaa8e0f found the earlier check, a 50 ms stamp, passing
// when the client was run later still).
func TestTheSendQueueStampsAChunkBeforeItHandsItOn(t *testing.T) {
	client := &stampOrder{}
	s := &AsyncSender{slots: make([][]byte, 1), free: make(chan int, 1), queue: make(chan sendItem, 1), watcher: client}
	client.s = s
	s.free <- 0
	if n, err := s.Write(make([]byte, secondOfAudio/4)); n != secondOfAudio/4 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if client.stamps != 1 || len(s.queue) != 1 {
		t.Fatalf("control: %d stamps and %d queued chunks, want one of each", client.stamps, len(s.queue))
	}
	if client.queuedAtStamp != 0 {
		t.Fatal("the send queue handed the chunk on before it stamped its capture")
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
