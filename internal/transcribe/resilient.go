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

// SetConnected resets backoff and marks connection as alive (called by Deepgram Open callback).
func (rc *ResilientClient) SetConnected() {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	rc.reconnectDelay = rc.Config.InitialReconnectDelay
	rc.setStateLocked(StateConnected)
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

// startReconnect starts a background reconnect goroutine (safe to call multiple times).
func (rc *ResilientClient) startReconnect() {
	rc.Mu.Lock()
	if rc.State == StateReconnecting {
		rc.Mu.Unlock()
		return
	}
	rc.setStateLocked(StateReconnecting)
	rc.Mu.Unlock()

	go rc.reconnect()
}

// reconnect handles reconnection with exponential backoff.
// Stops when rc.ctx is cancelled.
func (rc *ResilientClient) reconnect() {
	delay := rc.Config.InitialReconnectDelay

	for {
		select {
		case <-rc.ctx.Done():
			return
		case <-time.After(delay):
		}

		newClient, err := rc.Factory(rc.ctx)
		if err != nil {
			rc.log("deepgram reconnect failed (retrying in %v): %v", delay, err)
			delay *= 2
			if delay > rc.Config.MaxReconnectBackoff {
				delay = rc.Config.MaxReconnectBackoff
			}
			continue
		}

		rc.Mu.Lock()
		rc.Client = newClient
		rc.setStateLocked(StateDraining)
		rc.Mu.Unlock()

		rc.drainBuffer()

		rc.Mu.Lock()
		rc.setStateLocked(StateConnected)
		rc.reconnectDelay = rc.Config.InitialReconnectDelay
		rc.Mu.Unlock()

		rc.log("deepgram reconnected successfully")
		return
	}
}

// drainBuffer writes all buffered audio to the current client.
// Does NOT hold the mutex during external writes to avoid deadlock.
func (rc *ResilientClient) drainBuffer() {
	for {
		rc.Mu.Lock()
		if rc.Buffer.Len() == 0 {
			rc.Mu.Unlock()
			break
		}
		buf := make([]byte, 4096)
		n, _ := rc.Buffer.Read(buf)
		client := rc.Client
		rc.Mu.Unlock()

		if n == 0 || client == nil {
			break
		}
		if _, err := client.Write(buf[:n]); err != nil {
			rc.log("failed to drain buffered audio: %v", err)
		}
	}
}

// IsConnected returns true if the Deepgram connection is currently connected.
func (rc *ResilientClient) IsConnected() bool {
	rc.Mu.Lock()
	defer rc.Mu.Unlock()
	return rc.State == StateConnected
}
