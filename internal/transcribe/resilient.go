package transcribe

import (
	"context"
	"slices"
	"sort"
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
// Its audio is the latest captured, so its time marks the stream's clock
// (CapturedAt).
func (rc *ResilientClient) Write(p []byte) (int, error) {
	return rc.write(p, nil)
}

// writeFromQueue writes p, a chunk the send queue in front of the client
// holds, and takes it out of queued, the queue's count, as it counts it, under
// the same lock: Captured and CapturedSince then count the chunk once, never
// twice while a socket write stalls, and never not at all (sendQueueWatcher).
// The queue marked when p was captured as it took it (captured).
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
	} else {
		rc.markCapturedLocked(time.Now(), int64(len(p)))
	}
	client := rc.Client
	if rc.State != StateConnected || client == nil {
		// Buffered under the lock that counted p and read the state: a
		// reconnect in between would start its socket's stream len(p) too
		// late (CapturedSince), and a drain that ended in between would
		// leave p in the buffer, behind the audio sent after it.
		if writeErr := rc.bufferLocked(p); writeErr != nil {
			rc.log("failed to buffer audio while disconnected: %v", writeErr)
		}
		rc.Mu.Unlock()
		return len(p), nil
	}
	rc.Mu.Unlock()

	if _, err := client.Write(p); err == nil {
		return len(p), nil
	}

	rc.Mu.Lock()
	if writeErr := rc.bufferLocked(p); writeErr != nil {
		rc.log("failed to buffer audio after write error: %v", writeErr)
	}
	rc.setStateLocked(StateDisconnected)
	rc.Mu.Unlock()

	rc.startReconnect()

	return len(p), nil
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
	rc.setStateLocked(StateReconnecting)
	rc.Mu.Unlock()
	go rc.reconnect(0)
}

// startReconnect starts a background reconnect goroutine (safe to call multiple times).
func (rc *ResilientClient) startReconnect() {
	rc.Mu.Lock()
	if rc.State == StateReconnecting {
		rc.Mu.Unlock()
		return
	}
	rc.setStateLocked(StateReconnecting)
	rc.Mu.Unlock()

	go rc.reconnect(rc.Config.InitialReconnectDelay)
}

// reconnect dials after wait, then again with exponential backoff, until a
// socket is connected and the buffered audio sent. Stops when rc.ctx is
// cancelled. It first closes the socket that failed, which returns once that
// socket has handed on its last result: no result timed in the old socket's
// stream is then read against the new one's (CapturedSince).
func (rc *ResilientClient) reconnect(wait time.Duration) {
	next := rc.Config.InitialReconnectDelay
	if wait > 0 {
		next = min(2*wait, rc.Config.MaxReconnectBackoff)
	}
	backOff := func() {
		wait, next = next, min(2*next, rc.Config.MaxReconnectBackoff)
	}

	rc.Mu.Lock()
	failed := rc.Client
	rc.Mu.Unlock()
	if failed != nil {
		_ = failed.Close()
	}

	for {
		timer := time.NewTimer(wait)
		select {
		case <-rc.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

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
		rc.Client = newClient
		// The new socket's stream starts at the oldest buffered byte.
		rc.origin = rc.accepted - int64(rc.Buffer.Len())
		rc.keepDropsAfterLocked(rc.origin)
		rc.holes = rc.holes[:0]
		rc.setStateLocked(StateDraining)
		rc.Mu.Unlock()

		if err := rc.drainBuffer(); err != nil {
			// The new socket is dead too: keep the rest of the buffer for
			// the next one rather than throwing it into this one.
			rc.log("deepgram socket failed while draining buffered audio (redialling in %v): %v", next, err)
			rc.Mu.Lock()
			rc.setStateLocked(StateReconnecting)
			rc.Mu.Unlock()
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
// the first chunk the client fails to take, whose error it returns; that chunk
// is lost. Does NOT hold rc.Mu during external writes to avoid deadlock;
// drainMu covers the read and the write of each chunk, so Finalize waits for a
// chunk already read and no chunk is read after it.
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
// It makes the client connected under the lock that finds the buffer empty:
// a write before that is buffered for this drain and a write after it goes
// to the socket, so no audio is left in a buffer nothing drains.
func (rc *ResilientClient) drainChunk(buf []byte) (bool, error) {
	rc.drainMu.Lock()
	defer rc.drainMu.Unlock()

	rc.Mu.Lock()
	n, _ := rc.Buffer.Read(buf)
	client := rc.Client
	if n == 0 || client == nil {
		rc.setStateLocked(StateConnected)
	}
	rc.Mu.Unlock()

	if n == 0 || client == nil {
		return false, nil
	}
	if _, err := client.Write(buf[:n]); err != nil {
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
// Audio the stream skips, as Finalize or a full buffer dropped it unsent
// (holes), lies after the points before it and before the points after it.
func (rc *ResilientClient) CapturedSince(at time.Duration) time.Duration {
	rc.Mu.Lock()
	point := rc.pointLocked(at)
	since := rc.accepted - point
	if rc.queuedAhead != nil {
		since += rc.queuedAhead()
	}
	since += rc.droppedLocked() - rc.droppedUpToLocked(point)
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
	total := rc.capturedLocked()
	rc.Mu.Unlock()
	return rc.streamDuration(total)
}

// capturedLocked is Captured in bytes. It must be called with Mu held.
func (rc *ResilientClient) capturedLocked() int64 {
	total := rc.accepted + rc.forgottenDrops
	if rc.queuedAhead != nil {
		total += rc.queuedAhead()
	}
	total += rc.droppedLocked()
	return total
}

// CapturedBefore returns how much audio had been captured before the point
// at into the current socket's stream, on the count Captured gives: the audio
// the client took before it, Finalize's dropped audio included, and the audio
// the send queue dropped before it.
func (rc *ResilientClient) CapturedBefore(at time.Duration) time.Duration {
	rc.Mu.Lock()
	before := rc.capturedBeforeLocked(at)
	rc.Mu.Unlock()
	return rc.streamDuration(max(before, 0))
}

// capturedBeforeLocked is CapturedBefore in bytes. It must be called with Mu
// held.
func (rc *ResilientClient) capturedBeforeLocked(at time.Duration) int64 {
	point := rc.pointLocked(at)
	before := point + rc.forgottenDrops + rc.droppedUpToLocked(point)
	return before
}

// CapturedAt returns when the audio at the point at into the current
// socket's stream was captured, on the system clock: the time of the buffer
// that holds it, less the audio of that buffer after the point. Each Write,
// and each chunk the send queue in front of the client takes or drops, is
// stamped with when the microphone read it (AsyncSender.MarkRead), else when
// it came, on the count Captured gives. So a capture pause, which leaves a
// gap in time but none in the count, moves no word, nor does a check that
// held a buffer back after the microphone read it, nor when the time is
// asked, as when Deepgram's result for the point comes late. A point before
// the oldest stamp kept (captureStamps) was captured before the oldest audio
// the stamps hold, so it returns when that audio was captured: the latest
// the point can have been, never the arrival of its result. It reports
// false before any audio was stamped, for a point between two stamps, and
// for a point after the latest.
func (rc *ResilientClient) CapturedAt(at time.Duration) (time.Time, bool) {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	point := rc.capturedBeforeLocked(at)
	oldest := (rc.stampNext - rc.stampsN + captureStamps) % captureStamps
	stamp := func(i int) captureStamp { return rc.stamps[(oldest+i)%captureStamps] }
	// The stamps hold consecutive spans of the count, oldest first: the first
	// that ends after the point holds it, if any does.
	i := sort.Search(rc.stampsN, func(i int) bool { return stamp(i).to > point })
	if i == rc.stampsN || i > 0 && stamp(i).from > point {
		return time.Time{}, false
	}
	s := stamp(i)
	if s.from > point {
		point = s.from // older than the stamps kept: timed by their oldest audio
	}
	return s.at.Add(-rc.streamDuration(s.to - point)), true
}

// captureStamps is how many buffers CapturedAt times exactly: about 8.5
// minutes of the microphone's 250 ms buffers. That is more than the default
// buffer (DefaultBufferSeconds) lets the live stream fall behind, but a
// buffer set larger can outrun it: an older word is timed by the oldest
// audio kept, which it came before.
const captureStamps = 2048

// captureStamp is when a buffer was captured, and the span of the count
// Captured gives that it holds, in bytes.
type captureStamp struct {
	at       time.Time
	from, to int64
}

// markCapturedLocked stamps the latest n bytes Captured counts as captured
// by at (CapturedAt). It must be called with Mu held.
func (rc *ResilientClient) markCapturedLocked(at time.Time, n int64) {
	to := rc.capturedLocked()
	rc.stamps[rc.stampNext] = captureStamp{at: at, from: to - n, to: to}
	rc.stampNext = (rc.stampNext + 1) % captureStamps
	rc.stampsN = min(rc.stampsN+1, captureStamps)
}

// captured stamps the n bytes the send queue in front of the client just
// took or dropped, the latest audio counted, as captured by at
// (sendQueueWatcher).
func (rc *ResilientClient) captured(at time.Time, n int64) {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	rc.markCapturedLocked(at, n)
}

// pointLocked is where the point at into the current socket's stream lies in
// the count of the bytes Write takes: past each hole the stream skipped
// before it. It must be called with Mu held.
func (rc *ResilientClient) pointLocked(at time.Duration) int64 {
	p := rc.origin + rc.streamBytes(at)
	for _, h := range rc.holes { // oldest first, in the order the stream meets them
		if h.at >= rc.origin && h.at <= p {
			p += h.n
		}
	}
	return p
}

// bufferLocked buffers p for the next socket, or for the one draining. A
// full buffer drops its oldest audio, and the oldest of p itself when p is
// larger than the buffer, which no socket will be sent: a hole in the stream
// (holeLocked). It must be called with Mu held, after p was counted.
func (rc *ResilientClient) bufferLocked(p []byte) error {
	held := int64(rc.Buffer.Len())
	if lost := held + int64(len(p)) - int64(rc.Buffer.Cap()); lost > 0 {
		rc.holeLocked(rc.accepted-int64(len(p))-held, lost)
	}
	_, err := rc.Buffer.Write(p)
	return err
}

// holeLocked records that the current socket's stream skips the n bytes the
// client took from at, in the count Write takes: they were never sent. The
// holes last as long as the socket, so a point's place never moves. It must
// be called with Mu held.
func (rc *ResilientClient) holeLocked(at, n int64) {
	if last := len(rc.holes) - 1; last >= 0 && rc.holes[last].at+rc.holes[last].n == at {
		rc.holes[last].n += n
		return
	}
	rc.holes = append(rc.holes, streamDrop{at: at, n: n})
}

// streamDrop is audio a send queue in front of the client dropped, or a hole
// in the stream: n bytes that would have come at, in the count of the bytes
// Write takes. For a drop, sum is every drop's bytes up to and with it since
// the client began, which droppedUpToLocked reads.
type streamDrop struct{ at, n, sum int64 }

// droppedUpToLocked returns the bytes the send queue dropped at or before
// point that the current socket's stream still counts, in the count Write
// takes. It must be called with Mu held.
func (rc *ResilientClient) droppedUpToLocked(point int64) int64 {
	i := sort.Search(len(rc.drops), func(i int) bool { return rc.drops[i].at > point })
	if i == 0 {
		return 0
	}
	return rc.drops[i-1].sum - rc.dropsBase
}

// droppedLocked returns every drop the current socket's stream still counts.
// It must be called with Mu held.
func (rc *ResilientClient) droppedLocked() int64 {
	if len(rc.drops) == 0 {
		return 0
	}
	return rc.drops[len(rc.drops)-1].sum - rc.dropsBase
}

// watchSendQueue makes CapturedSince count the audio an AsyncSender in
// front of the client holds (sendQueueWatcher).
func (rc *ResilientClient) watchSendQueue(queued func() int64) {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	rc.queuedAhead = queued
}

// sendQueueDropped records n bytes the send queue in front of the client
// dropped: they come after every byte taken or queued so far
// (sendQueueWatcher). Every drop is kept while the socket lasts, so a
// point's count never moves.
func (rc *ResilientClient) sendQueueDropped(n int64) {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	at := rc.accepted
	if rc.queuedAhead != nil {
		at += rc.queuedAhead()
	}
	sum := rc.dropsBase + n
	if last := len(rc.drops) - 1; last >= 0 {
		if rc.drops[last].at == at {
			rc.drops[last].n += n
			rc.drops[last].sum += n
			return
		}
		sum = rc.drops[last].sum + n
	}
	rc.drops = append(rc.drops, streamDrop{at: at, n: n, sum: sum})
}

// keepDropsAfterLocked forgets the drops at or before from, the start of a
// new socket's stream: no word Deepgram hears on it comes before them. Their
// bytes still count in Captured and CapturedBefore. It must be called with Mu
// held.
func (rc *ResilientClient) keepDropsAfterLocked(from int64) {
	i := sort.Search(len(rc.drops), func(i int) bool { return rc.drops[i].at > from })
	if i == 0 {
		return
	}
	rc.forgottenDrops += rc.drops[i-1].sum - rc.dropsBase
	rc.dropsBase = rc.drops[i-1].sum
	rc.drops = append([]streamDrop(nil), rc.drops[i:]...)
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
// buffered chunk the reconnect is already writing, then asks Deepgram to
// return final results for the audio the live socket has. A capture pause
// calls it, so no audio reaches Deepgram while paused and audio from before
// the pause comes back before the pause ends, not after it.
func (rc *ResilientClient) Finalize() error {
	rc.Mu.Lock()
	// The live socket, still draining, would have been sent these next: its
	// stream skips them, after the audio already sent.
	if n := int64(rc.Buffer.Len()); n > 0 {
		rc.holeLocked(rc.accepted-n, n)
	}
	rc.Buffer.Reset()
	rc.Mu.Unlock()

	rc.drainMu.Lock()
	defer rc.drainMu.Unlock()
	rc.Mu.Lock()
	state := rc.State
	client := rc.Client
	rc.Mu.Unlock()

	// Connected or draining, Client is the live socket. Disconnected or
	// reconnecting, it is the one that failed, which the SDK would redial; the
	// reset above already keeps its audio from the next socket.
	if state != StateConnected && state != StateDraining {
		return nil
	}
	finalizer, ok := client.(interface{ Finalize() error })
	if !ok {
		return nil
	}
	return finalizer.Finalize()
}
