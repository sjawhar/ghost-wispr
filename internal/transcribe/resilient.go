package transcribe

import (
	"context"
	"time"
)

// NewResilientClient creates a new ResilientClient that wraps a Deepgram client
// with automatic reconnection and audio buffering during disconnects.
func NewResilientClient(
	ctx context.Context,
	factory ClientFactory,
	config ResilientConfig,
	logger func(string, ...any),
	onStateChange ConnectionCallback,
) *ResilientClient {
	if config.BufferSize <= 0 {
		config.BufferSize = 60 * 32000
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
	state := rc.State
	client := rc.Client
	rc.Mu.Unlock()

	if state == StateConnected && client != nil {
		_, err := client.Write(p)
		if err == nil {
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

	rc.Mu.Lock()
	if _, writeErr := rc.Buffer.Write(p); writeErr != nil {
		rc.log("failed to buffer audio while disconnected: %v", writeErr)
	}
	rc.Mu.Unlock()

	return len(p), nil
}

// Close stops reconnection attempts and closes the underlying client.
func (rc *ResilientClient) Close() error {
	rc.cancel()

	rc.Mu.Lock()
	defer rc.Mu.Unlock()

	if rc.Client != nil {
		return rc.Client.Close()
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
// cancelled.
func (rc *ResilientClient) reconnect(wait time.Duration) {
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

		newClient, err := rc.Factory(rc.ctx)
		if err != nil {
			rc.log("deepgram reconnect failed (retrying in %v): %v", next, err)
			backOff()
			continue
		}

		rc.Mu.Lock()
		rc.Client = newClient
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
		rc.setStateLocked(StateConnected)
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
func (rc *ResilientClient) drainChunk(buf []byte) (bool, error) {
	rc.drainMu.Lock()
	defer rc.drainMu.Unlock()

	rc.Mu.Lock()
	n, _ := rc.Buffer.Read(buf)
	client := rc.Client
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

// Finalize ends the current stretch of audio. It drops the audio buffered
// during a disconnect, which a reconnect would otherwise send, waits for a
// buffered chunk the reconnect is already writing, then asks Deepgram to
// return final results for the audio the live socket has. A capture pause
// calls it, so no audio reaches Deepgram while paused and audio from before
// the pause comes back before the pause ends, not after it.
func (rc *ResilientClient) Finalize() error {
	rc.Mu.Lock()
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
