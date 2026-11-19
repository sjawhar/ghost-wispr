package transcribe

import (
	"context"
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
func (rc *ResilientClient) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	rc.Mu.Lock()
	rc.accepted += int64(len(p))
	client := rc.Client
	if rc.State != StateConnected || client == nil {
		// Buffered under the lock that counted p and read the state: a
		// reconnect in between would start its socket's stream len(p) too
		// late (CapturedSince), and a drain that ended in between would
		// leave p in the buffer, behind the audio sent after it.
		if _, writeErr := rc.Buffer.Write(p); writeErr != nil {
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
	if _, writeErr := rc.Buffer.Write(p); writeErr != nil {
		rc.log("failed to buffer audio after write error: %v", writeErr)
	}
	rc.setStateLocked(StateDisconnected)
	rc.Mu.Unlock()

	rc.startReconnect()

	return len(p), nil
}

// Close stops reconnection attempts and closes the underlying client. It
// closes the client without holding Mu: a socket's close waits for its
// reader, which may be timing a result's words (CapturedSince), and that
// takes Mu.
func (rc *ResilientClient) Close() error {
	rc.cancel()

	rc.Mu.Lock()
	client := rc.Client
	rc.Mu.Unlock()

	if client != nil {
		return client.Close()
	}
	return nil
}

// setStateLocked updates state and invokes callback. Must be called with rc.Mu held.
func (rc *ResilientClient) setStateLocked(newState ConnectionState) {
	if rc.State == newState {
		return
	}
	rc.State = newState
	if rc.OnStateChange != nil {
		go rc.OnStateChange(newState)
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
// buffered chunk the reconnect is already writing, then asks Deepgram to
// return final results for the audio the live socket has. A capture pause
// calls it, so no audio reaches Deepgram while paused and audio from before
// the pause comes back before the pause ends, not after it.
func (rc *ResilientClient) Finalize() error {
	rc.Mu.Lock()
	// The live socket, still draining, would have been sent these next.
	rc.origin += int64(rc.Buffer.Len())
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
