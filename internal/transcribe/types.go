package transcribe

import (
	"context"
	"io"
	"sync"
	"time"
)

// ConnectionState represents the state of the Deepgram websocket connection.
type ConnectionState int

const (
	StateConnected ConnectionState = iota
	StateDisconnected
	StateReconnecting
	StateDraining
)

// String returns a human-readable representation of the connection state.
func (s ConnectionState) String() string {
	switch s {
	case StateConnected:
		return "connected"
	case StateDisconnected:
		return "disconnected"
	case StateReconnecting:
		return "reconnecting"
	case StateDraining:
		return "draining"
	default:
		return "unknown"
	}
}

// ResilientConfig holds configuration for the ResilientClient.
type ResilientConfig struct {
	// BufferSize is the ring buffer capacity in bytes.
	// Default: 60 * 32000 = 1,920,000 (60 seconds at 16kHz 16-bit mono PCM = 32 KB/sec)
	BufferSize int

	// MaxReconnectBackoff is the maximum delay between reconnection attempts.
	// Default: 30 seconds
	MaxReconnectBackoff time.Duration

	// InitialReconnectDelay is the initial delay before the first reconnection attempt.
	// Default: 500 milliseconds
	InitialReconnectDelay time.Duration
}

// ClientFactory is a function type that creates a new Deepgram client.
// It decouples the ResilientClient from SDK specifics and enables testing with mock factories.
type ClientFactory func(ctx context.Context) (io.WriteCloser, error)

// ConnectionCallback is a function type for state change notifications.
// It's called whenever the connection state changes (e.g., for logging or metrics).
type ConnectionCallback func(state ConnectionState)

// ResilientClient wraps an io.WriteCloser (Deepgram client) and buffers audio during disconnects.
// All fields are exported for testing purposes.
type ResilientClient struct {
	// State is the current connection state.
	State ConnectionState

	// Buffer is the ring buffer for audio data during disconnects.
	Buffer *RingBuf

	// Mu protects all mutable state (State, reconnect logic, buffer access).
	Mu sync.Mutex

	// Config holds configuration for buffer size and reconnection backoff.
	Config ResilientConfig

	// Factory creates new Deepgram clients on reconnection.
	Factory ClientFactory

	// Client is the current underlying io.WriteCloser (Deepgram SDK client).
	Client io.WriteCloser

	// OnStateChange is an optional callback for state transitions.
	OnStateChange ConnectionCallback

	// ctx is the application context; cancellation stops the reconnect loop.
	ctx    context.Context
	// cancel cancels ctx, stopping the reconnect goroutine on Close().
	cancel context.CancelFunc
	// log is a printf-style logger.
	log    func(string, ...any)

	// reconnectDelay tracks the current exponential backoff delay.
	reconnectDelay time.Duration
}
