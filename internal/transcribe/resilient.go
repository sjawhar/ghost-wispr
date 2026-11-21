package transcribe

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"time"
)

// DefaultBufferSeconds is how much audio the ResilientClient holds while it
// reconnects, when ResilientConfig sets no BufferSize.
const DefaultBufferSeconds = 60

// defaultBufferRate is the sample rate of the audio when ResilientConfig sets
// none.
const defaultBufferRate = 16000

// bytesPerSample is the size of a sample of the 16-bit mono PCM written.
const bytesPerSample = 2

// NewResilientClient creates a new ResilientClient that wraps a Deepgram client
// with automatic reconnection and audio buffering during disconnects.
func NewResilientClient(
	ctx context.Context,
	factory ClientFactory,
	config ResilientConfig,
	logger func(string, ...any),
	onStateChange ConnectionCallback,
) *ResilientClient {
	if config.SampleRate <= 0 {
		config.SampleRate = defaultBufferRate
	}
	if config.BufferSize <= 0 {
		config.BufferSize = DefaultBufferSeconds * config.SampleRate * bytesPerSample
	}
	if config.InitialReconnectDelay <= 0 {
		config.InitialReconnectDelay = 500 * time.Millisecond
	}
	if config.MaxReconnectBackoff <= 0 {
		config.MaxReconnectBackoff = 30 * time.Second
	}
	if config.GracefulCloseWait <= 0 {
		config.GracefulCloseWait = DefaultGracefulCloseWait
	}

	if logger == nil {
		logger = func(string, ...any) {}
	}

	ctx, cancel := context.WithCancel(ctx)
	return &ResilientClient{
		State:          StateConnected,
		Buffer:         NewRingBuf(config.BufferSize),
		Config:         config,
		Factory:        factory,
		OnStateChange:  onStateChange,
		ctx:            ctx,
		cancel:         cancel,
		log:            logger,
		reconnectDelay: config.InitialReconnectDelay,
	}
}

// Write always returns (len(p), nil) — error absorption is the whole point.
func (rc *ResilientClient) Write(p []byte) (int, error) {
	return rc.write(p, nil)
}

// writeFromQueue writes p, a chunk the send queue in front of the client
// holds, and takes it out of queued, the queue's count, as it counts it, under
// the same lock: Captured and CapturedSince then count the chunk once, never
// twice while a socket write stalls, and never not at all (sendQueueWatcher).
func (rc *ResilientClient) writeFromQueue(p []byte, queued *atomic.Int64) (int, error) {
	return rc.write(p, queued)
}

func (rc *ResilientClient) write(p []byte, queued *atomic.Int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	rc.Mu.Lock()
	rc.accepted += int64(len(p))
	if queued != nil {
		queued.Add(-int64(len(p)))
	}
	client := rc.Client
	if rc.State != StateConnected || client == nil {
		// Buffered under the lock that counted p and read the state: a
		// reconnect in between would start its socket's stream len(p) too
		// late (CapturedSince), and a drain that ended in between would
		// leave p in the buffer, behind the audio sent after it.
		rc.bufferLocked(p)
		rc.Mu.Unlock()
		return len(p), nil
	}
	rc.Mu.Unlock()

	for {
		if _, err := client.Write(p); err == nil {
			return len(p), nil
		}

		rc.Mu.Lock()
		if rc.Client == client {
			// The socket that failed is the client's: it is broken.
			rc.bufferLocked(p)
			rc.setStateLocked(StateDisconnected)
			rc.Mu.Unlock()
			rc.startReconnect()
			return len(p), nil
		}
		// The socket was replaced while p was on its way, as after an options
		// change: the failure is the old socket's, not the client's. p goes
		// where the client sends audio now: the buffer, which the reconnect
		// owner still drains, or the successor, once it is connected.
		if rc.State != StateConnected || rc.Client == nil {
			rc.bufferLocked(p)
			rc.Mu.Unlock()
			return len(p), nil
		}
		client = rc.Client
		rc.Mu.Unlock()
	}
}

// bufferLocked keeps p for the next socket, behind the audio kept already: the
// chunk a drain write has pending, then the buffer. What is kept is bounded by
// the buffer's size and is always one tail of the capture: when p does not
// fit, the oldest audio goes first, the pending chunk's front before the
// buffer's, so one start of a socket's stream maps all of it to the times it
// was captured. Called with Mu held.
func (rc *ResilientClient) bufferLocked(p []byte) {
	if over := len(rc.pending) + rc.Buffer.Len() + len(p) - rc.Buffer.Cap(); over > 0 {
		fromPending := min(over, len(rc.pending))
		rc.pending = rc.pending[fromPending:]
		// When p alone is longer than the buffer, the buffer's write drops
		// p's front: the rest of over.
		rc.Buffer.Discard(over - fromPending)
		rc.droppedLocked(int64(over))
	}
	_, _ = rc.Buffer.Write(p)
}

// droppedLocked notes that an overflow dropped the d oldest bytes of the kept
// audio, while the client may be draining it into a socket, whose stream must
// not jump over them. During a drain write the count waits for the write's
// end, as the socket may take some of the bytes dropped (drainChunk). Before
// the socket took any audio, its stream starts d bytes later. After, its
// stream would jump, so it ends at its next chunk. Called with Mu held.
func (rc *ResilientClient) droppedLocked(d int64) {
	if rc.State != StateDraining {
		return // no socket's stream holds them: the next dial starts at the tail
	}
	switch {
	case rc.inFlight:
		rc.droppedInFlight += d
	case rc.took == 0:
		rc.origin += d
		rc.keepDropsAfterLocked(rc.origin)
	default:
		rc.gap = true
	}
}

// keptFromLocked is where the audio kept starts, in the count of the bytes
// Write takes: the pending chunk, then the buffer, are the last bytes taken.
// Called with Mu held.
func (rc *ResilientClient) keptFromLocked() int64 {
	return rc.accepted - int64(rc.Buffer.Len()) - int64(len(rc.pending))
}

// Close stops reconnection attempts and the hand-over of state changes, and
// closes the underlying client. It closes the client without holding Mu: a
// socket's close waits for its reader, which may be timing a result's words
// (CapturedSince), and that takes Mu. It then waits for the state-change
// goroutine to end, which takes at most stateCallbackStopWait (1 s): the
// callback in progress gets the cancelled context, and one that does not
// return in time is left running, logged, rather than holding Close. The
// Deepgram status callback stores its event in SQLite, and a store already
// waiting on another connection's write lock does not heed the cancel; it
// finishes or fails within SQLite's busy_timeout (5 s).
func (rc *ResilientClient) Close() error {
	rc.cancel()

	rc.Mu.Lock()
	client := rc.Client
	notifierDone := rc.notifierDone
	rc.Mu.Unlock()

	var err error
	if client != nil {
		err = client.Close()
	}
	if notifierDone != nil {
		<-notifierDone
	}
	return err
}

// maxQueuedStateChanges bounds the changes queued for OnStateChange while it
// is behind: a stuck callback must not grow the queue without end.
const maxQueuedStateChanges = 64

// stateCallbackStopWait is how long a closed client waits for the state
// callback in progress to return.
const stateCallbackStopWait = time.Second

// stateSeqs numbers the state changes of every client in the process, so a
// listener that outlives one client (the hub, across a new microphone start)
// never reads a new client's change as older than an old client's.
var stateSeqs atomic.Uint64

// CurrentState returns the client's state with the number of the change that
// made it: what a listener showing the current state applies, rather than the
// older change it may be hearing of.
func (rc *ResilientClient) CurrentState() StateChange {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	return StateChange{Seq: rc.stateSeq, State: rc.State}
}

// setStateLocked updates state, numbers the change, applies it to
// OnStateChangeLocked, and queues it for OnStateChange. Must be called with
// rc.Mu held. It never waits for OnStateChange: Write calls it on the capture
// path.
func (rc *ResilientClient) setStateLocked(newState ConnectionState) {
	if rc.State == newState {
		return
	}
	rc.State = newState
	rc.stateSeq = stateSeqs.Add(1)
	change := StateChange{Seq: rc.stateSeq, State: newState}
	if rc.OnStateChangeLocked != nil {
		rc.OnStateChangeLocked(change)
	}
	if rc.OnStateChange == nil || rc.ctx.Err() != nil {
		return
	}
	if len(rc.stateChanges) == maxQueuedStateChanges {
		rc.stateChanges = slices.Delete(rc.stateChanges, 0, 1)
	}
	rc.stateChanges = append(rc.stateChanges, change)
	if !rc.notifying {
		rc.notifying = true
		rc.notifierDone = make(chan struct{})
		go rc.notifyStateChanges(rc.notifierDone)
	}
}

// notifyStateChanges hands the queued changes to OnStateChange one at a time,
// oldest first, with Mu released during each call, and ends, closing done,
// once none is left or the client closes. Each change used to get a goroutine
// of its own, so two quick changes could reach the callback reversed: on the
// office Pi on 2026-11-20 a resume's draining and connected reached the web
// clients in the wrong order, and every client held "draining" while Deepgram
// was connected. Each call runs on a goroutine of its own, which this one
// waits for, so that a close can end this one while a callback ignores the
// context.
func (rc *ResilientClient) notifyStateChanges(done chan struct{}) {
	defer close(done)
	for {
		rc.Mu.Lock()
		if len(rc.stateChanges) == 0 || rc.ctx.Err() != nil {
			rc.stateChanges = nil
			rc.notifying = false
			rc.Mu.Unlock()
			return
		}
		change := rc.stateChanges[0]
		rc.stateChanges = slices.Delete(rc.stateChanges, 0, 1)
		rc.Mu.Unlock()

		returned := make(chan struct{})
		go func() {
			defer close(returned)
			rc.OnStateChange(rc.ctx, change)
		}()
		select {
		case <-returned:
		case <-rc.ctx.Done():
			select {
			case <-returned:
			case <-time.After(stateCallbackStopWait):
				rc.log("deepgram: the state callback for change %d (%s) did not return within %v of the close; it is left running", change.Seq, change.State, stateCallbackStopWait)
			}
		}
	}
}

// ConnectInBackground dials the first socket on a goroutine of its own and
// returns at once: a dial can take as long as the handshake limit, and a
// microphone start, which holds the recording light red until it returns,
// must not wait for it. Until a socket is connected the client is
// reconnecting and buffers the audio written meanwhile, which it sends once
// one is, and a failed dial is retried with backoff, as after a disconnect.
func (rc *ResilientClient) ConnectInBackground() {
	rc.Mu.Lock()
	rc.owning = true
	rc.setStateLocked(StateReconnecting)
	rc.Mu.Unlock()
	go rc.reconnect(0)
}

// OptionsChanged tells the client its factory's options changed, such as the
// meeting voice turned on or off changing no_delay. It only marks the change:
// the one reconnect owner redials once with the new options at its next safe
// point, losing no audio. A dial or a drain under way finishes first, and the
// owner redials after it if its dial predates the change. With no owner
// running, the client is connected, and the change starts one, which rotates
// the socket: the audio written from now on is buffered, the old socket ends
// its stream gracefully and returns its final results (GracefulCloser), then
// a new socket is dialled with the current options and the buffer drained
// into it.
func (rc *ResilientClient) OptionsChanged() {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	rc.optionsGen++
	if rc.owning || rc.ctx.Err() != nil {
		return // the owner redials at its next safe point
	}
	rc.owning = true
	rc.setStateLocked(StateReconnecting) // the audio written from here is buffered
	go rc.rotate()
}

// startReconnect starts the reconnect owner after a socket failed, unless one
// runs already: there is only ever one (owning).
func (rc *ResilientClient) startReconnect() {
	rc.Mu.Lock()
	if rc.owning || rc.ctx.Err() != nil {
		rc.Mu.Unlock()
		return
	}
	rc.owning = true
	rc.setStateLocked(StateReconnecting)
	rc.Mu.Unlock()

	go rc.reconnect(rc.Config.InitialReconnectDelay)
}

// reconnect is the owner after a socket failed: it closes that socket, which
// returns once the socket has handed on its last result, so no result timed in
// the old socket's stream is read against the new one's (CapturedSince), then
// dials until a socket is connected and the buffered audio sent.
func (rc *ResilientClient) reconnect(wait time.Duration) {
	rc.Mu.Lock()
	rc.owning = true
	failed := rc.Client
	rc.Mu.Unlock()
	if failed != nil {
		_ = failed.Close()
	}
	rc.dialUntilConnected(wait)
}

// rotate is the owner after an options change replaced a healthy socket: the
// old socket ends its stream gracefully and returns its final results before
// the new one is dialled, so the audio it accepted is transcribed, and the
// audio written since the change, buffered, goes to the new socket.
func (rc *ResilientClient) rotate() {
	rc.Mu.Lock()
	old := rc.Client
	rc.Mu.Unlock()
	if old != nil {
		rc.closeGracefully(old)
	}
	rc.dialUntilConnected(0)
}

// closeGracefully ends c's stream gracefully when it can (GracefulCloser),
// within GracefulCloseWait, and closes it at once otherwise.
func (rc *ResilientClient) closeGracefully(c io.WriteCloser) {
	if graceful, ok := c.(GracefulCloser); ok {
		_ = graceful.CloseGracefully(rc.Config.GracefulCloseWait)
		return
	}
	_ = c.Close()
}

// errOptionsChanged is what a drain reports when it emptied the buffer into a
// socket whose dial predates an options change: the owner then rotates.
var errOptionsChanged = errors.New("the options changed since the socket was dialled")

// errStreamGap is what a drain reports when an overflow dropped audio the
// socket had not taken, after audio it had: one start of its stream can no
// longer map both to the times they were captured, so the owner rotates.
var errStreamGap = errors.New("the buffer overflowed past the audio the socket took")

// dialUntilConnected dials after wait, then again with exponential backoff,
// until a socket is connected and the buffered audio sent; it stops when rc.ctx
// is cancelled. Each dial reads the options the factory builds then, and the
// generation of the options it saw: right after the handshake, and when the
// drain empties the buffer, a later options change makes the owner end that
// socket and dial once more, so the client never stays on stale options. No
// audio is lost to it: the buffer keeps everything not yet sent.
func (rc *ResilientClient) dialUntilConnected(wait time.Duration) {
	next := rc.Config.InitialReconnectDelay
	if wait > 0 {
		next = min(2*wait, rc.Config.MaxReconnectBackoff)
	}
	backOff := func() {
		wait, next = next, min(2*next, rc.Config.MaxReconnectBackoff)
	}

	for {
		timer := time.NewTimer(wait)
		select {
		case <-rc.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		// The generation is read before the factory reads the options, so a
		// change between them makes one extra redial, never a missed one.
		rc.Mu.Lock()
		gen := rc.optionsGen
		rc.Mu.Unlock()
		newClient, err := rc.Factory(rc.ctx)
		if err != nil {
			rc.log("deepgram reconnect failed (retrying in %v): %v", next, err)
			backOff()
			continue
		}

		rc.Mu.Lock()
		if rc.ctx.Err() != nil {
			// Close ran during the dial, and closed the client it saw.
			rc.Mu.Unlock()
			_ = newClient.Close()
			return
		}
		if rc.optionsGen != gen {
			// The options changed during the handshake: this socket has had
			// no audio, so it ends at once and the owner dials again.
			rc.Mu.Unlock()
			rc.log("deepgram: the options changed during the dial; dialling again with them")
			_ = newClient.Close()
			wait = 0
			continue
		}
		rc.Client = newClient
		rc.dialedGen = gen
		// The new socket's stream starts at the oldest byte kept: the chunk a
		// failed drain kept, then the buffer, one tail of the capture.
		rc.origin = rc.keptFromLocked()
		rc.keepDropsAfterLocked(rc.origin)
		rc.took, rc.inFlight, rc.droppedInFlight, rc.gap = 0, false, 0, false
		rc.setStateLocked(StateDraining)
		rc.Mu.Unlock()

		if err := rc.drainBuffer(); err != nil {
			rc.Mu.Lock()
			rc.setStateLocked(StateReconnecting)
			rc.Mu.Unlock()
			if errors.Is(err, errOptionsChanged) {
				// The buffer went to this socket, which the change made
				// stale: it returns its final results, then the owner dials
				// again for the audio buffered since.
				rc.log("deepgram: the options changed during the drain; redialling with them")
				rc.closeGracefully(newClient)
				wait = 0
				continue
			}
			// The new socket is dead too, or the capture outran it by a whole
			// buffer, so an overflow dropped audio it had not taken
			// (errStreamGap): it ends at once, as a socket that fell behind
			// would not return its last results in time, and the next one
			// starts at the audio kept.
			rc.log("deepgram socket failed while draining buffered audio (redialling in %v): %v", next, err)
			_ = newClient.Close()
			backOff()
			continue
		}

		rc.Mu.Lock()
		rc.reconnectDelay = rc.Config.InitialReconnectDelay
		rc.Mu.Unlock()

		rc.log("deepgram reconnected successfully")
		return
	}
}

// drainBuffer writes all buffered audio to the current client, and stops at
// the first chunk the client fails to take, whose error it returns; the bytes
// of that chunk the client did not take stay pending, and the next socket's
// drain sends them first. Does NOT hold rc.Mu during external writes to avoid
// deadlock; drainMu covers the read and the write of each chunk, so Finalize
// waits for a chunk already read and no chunk is read after it.
func (rc *ResilientClient) drainBuffer() error {
	buf := make([]byte, 4096)
	for {
		more, err := rc.drainChunk(buf)
		if err != nil || !more {
			return err
		}
	}
}

// drainChunk writes one buffered chunk and reports whether to drain another.
// The chunk is the one a failed write left pending, if any, else the next out
// of the buffer, and it stays pending until the client took all of it: a write
// that fails, wholly or in part, leaves the bytes it did not send for the next
// socket, before any later audio. It makes the client connected, and ends the
// owner's run, under the lock that finds nothing left to send: a write before
// that is buffered for this drain and a write after it goes to the socket, so
// no audio is left in a buffer nothing drains. If the options changed since
// this socket's dial, it leaves the client buffering and reports
// errOptionsChanged instead, so the owner, still the only one, rotates: the
// check and the hand-over to connected are one step, so no change is lost
// between them. If an overflow dropped audio this socket had not taken, after
// audio it had, it reports errStreamGap: its stream ends whole there, and the
// next socket's starts at the audio kept.
func (rc *ResilientClient) drainChunk(buf []byte) (bool, error) {
	rc.drainMu.Lock()
	defer rc.drainMu.Unlock()

	rc.Mu.Lock()
	if rc.gap && rc.ctx.Err() == nil {
		// An overflow dropped audio this socket had not taken, after audio it
		// had: its stream would jump over it.
		rc.Mu.Unlock()
		return false, errStreamGap
	}
	if len(rc.pending) == 0 {
		n, _ := rc.Buffer.Read(buf)
		rc.pending = append(rc.pending[:0], buf[:n]...)
	}
	chunk := rc.pending
	client := rc.Client
	if len(chunk) == 0 || client == nil {
		if client != nil && rc.optionsGen != rc.dialedGen && rc.ctx.Err() == nil {
			rc.Mu.Unlock()
			return false, errOptionsChanged
		}
		rc.setStateLocked(StateConnected)
		rc.owning = false
	} else {
		rc.inFlight, rc.droppedInFlight = true, 0
	}
	rc.Mu.Unlock()

	if len(chunk) == 0 || client == nil {
		return false, nil
	}
	n, err := client.Write(chunk)
	n = max(0, min(n, len(chunk)))
	rc.Mu.Lock()
	rc.inFlight = false
	rc.took += int64(n)
	// An overflow during the write dropped the oldest kept bytes, the chunk's
	// front first. If the socket took at least as many, it took them all: the
	// chunk's bytes it took beyond them are sent, and its stream goes on with
	// no gap. Else it skipped audio the overflow dropped, and it ends at its
	// next chunk.
	if dropped := rc.droppedInFlight; dropped <= int64(n) {
		rc.pending = rc.pending[n-int(dropped):]
	} else {
		rc.gap = true
	}
	rc.droppedInFlight = 0
	rc.Mu.Unlock()
	if err == nil && n < len(chunk) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// IsConnected returns true if the Deepgram connection is currently connected.
func (rc *ResilientClient) IsConnected() bool {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	return rc.State == StateConnected
}

// CapturedSince returns how much audio was captured after the point at into
// the current socket's stream, the time Deepgram gives the words it hears:
// the audio sent to that socket since, the audio buffered for it or for the
// next, the audio queued in front of the client (AsyncSender), and the audio
// that queue dropped after that point, which never reached the socket.
// Audio Finalize drops from the buffer is taken out of the stream, as it
// never reaches the socket.
func (rc *ResilientClient) CapturedSince(at time.Duration) time.Duration {
	rc.Mu.Lock()
	point := rc.origin + rc.streamBytes(at)
	since := rc.accepted - point
	if rc.queuedAhead != nil {
		since += rc.queuedAhead()
	}
	for _, drop := range rc.drops {
		if drop.at > point {
			since += drop.n
		}
	}
	rc.Mu.Unlock()
	if since <= 0 {
		return 0
	}
	return rc.streamDuration(since)
}

// Captured returns how much audio has been captured in all: the audio the
// client took, the audio queued in front of it, and the audio that queue
// dropped. CapturedBefore places a point of the current socket's stream on
// the same count, which no reconnect, Finalize or drop moves.
func (rc *ResilientClient) Captured() time.Duration {
	rc.Mu.Lock()
	total := rc.accepted + rc.forgottenDrops
	if rc.queuedAhead != nil {
		total += rc.queuedAhead()
	}
	for _, drop := range rc.drops {
		total += drop.n
	}
	rc.Mu.Unlock()
	return rc.streamDuration(total)
}

// CapturedBefore returns how much audio had been captured before the point
// at into the current socket's stream, on the count Captured gives: the audio
// the client took before it, Finalize's dropped audio included, and the audio
// the send queue dropped before it.
func (rc *ResilientClient) CapturedBefore(at time.Duration) time.Duration {
	rc.Mu.Lock()
	point := rc.origin + rc.streamBytes(at)
	before := point + rc.forgottenDrops
	for _, drop := range rc.drops {
		if drop.at <= point {
			before += drop.n
		}
	}
	rc.Mu.Unlock()
	return rc.streamDuration(max(before, 0))
}

// streamDrop is audio a send queue in front of the client dropped: n bytes
// that would have come at, in the count of the bytes Write takes.
type streamDrop struct{ at, n int64 }

// maxStreamDrops bounds the drops the client remembers: past it, the oldest
// two merge into one at the later place, which counts the earlier one's
// bytes after a few more words than it should. That only lengthens a
// recording's lead, and can only place a word between them earlier than it
// was captured.
const maxStreamDrops = 1024

// watchSendQueue makes CapturedSince count the audio an AsyncSender in
// front of the client holds (sendQueueWatcher).
func (rc *ResilientClient) watchSendQueue(queued func() int64) {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	rc.queuedAhead = queued
}

// sendQueueDropped records n bytes the send queue in front of the client
// dropped: they come after every byte taken or queued so far
// (sendQueueWatcher).
func (rc *ResilientClient) sendQueueDropped(n int64) {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	at := rc.accepted
	if rc.queuedAhead != nil {
		at += rc.queuedAhead()
	}
	if last := len(rc.drops) - 1; last >= 0 && rc.drops[last].at == at {
		rc.drops[last].n += n
		return
	}
	if len(rc.drops) == maxStreamDrops {
		rc.drops[1].n += rc.drops[0].n
		rc.drops = rc.drops[1:]
	}
	rc.drops = append(rc.drops, streamDrop{at: at, n: n})
}

// keepDropsAfterLocked forgets the drops at or before from, the start of a
// new socket's stream: no word Deepgram hears on it comes before them. Their
// bytes still count in Captured and CapturedBefore. It must be called with Mu
// held.
func (rc *ResilientClient) keepDropsAfterLocked(from int64) {
	kept := rc.drops[:0]
	for _, drop := range rc.drops {
		if drop.at > from {
			kept = append(kept, drop)
		} else {
			rc.forgottenDrops += drop.n
		}
	}
	rc.drops = kept
}

// MaxBehind returns how far the stream Deepgram hears can fall behind the
// audio written: the most audio the client buffers while it is not
// connected.
func (rc *ResilientClient) MaxBehind() time.Duration {
	return rc.streamDuration(int64(rc.Buffer.Cap()))
}

// streamBytes is the size of d of the audio written, in whole samples. It
// splits d at the second, so a socket's stream of any length does not
// overflow it.
func (rc *ResilientClient) streamBytes(d time.Duration) int64 {
	rate := int64(rc.Config.SampleRate)
	samples := int64(d/time.Second)*rate + int64(d%time.Second)*rate/int64(time.Second)
	return samples * bytesPerSample
}

// streamDuration is how long n bytes of the audio written last. It splits n
// at the second, so a count of bytes the client can take does not overflow
// it.
func (rc *ResilientClient) streamDuration(n int64) time.Duration {
	perSecond := int64(rc.Config.SampleRate) * bytesPerSample
	return time.Duration(n/perSecond)*time.Second + time.Duration(n%perSecond)*time.Second/time.Duration(perSecond)
}

// Finalize ends the current stretch of audio. It drops the audio buffered
// during a disconnect, which a reconnect would otherwise send, waits for a
// buffered chunk the reconnect is already writing, drops what of a chunk a
// failed write left pending, then asks Deepgram to return final results for
// the audio the live socket has. A capture pause calls it, so no audio reaches
// Deepgram while paused and audio from before the pause comes back before the
// pause ends, not after it.
func (rc *ResilientClient) Finalize() error {
	rc.Mu.Lock()
	// The live socket, still draining, would have been sent these next.
	rc.origin += int64(rc.Buffer.Len())
	rc.Buffer.Reset()
	rc.Mu.Unlock()

	rc.drainMu.Lock()
	defer rc.drainMu.Unlock()
	rc.Mu.Lock()
	// The pending chunk is older than the buffer, and as much from before the
	// pause: the live socket would have been sent it first.
	rc.origin += int64(len(rc.pending))
	rc.pending = rc.pending[:0]
	state := rc.State
	client := rc.Client
	rc.Mu.Unlock()

	// Connected or draining, Client is the live socket. Disconnected or
	// reconnecting, it is the one that failed, which the SDK would redial; the
	// resets above already keep its audio from the next socket.
	if state != StateConnected && state != StateDraining {
		return nil
	}
	finalizer, ok := client.(interface{ Finalize() error })
	if !ok {
		return nil
	}
	return finalizer.Finalize()
}
