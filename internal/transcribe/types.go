package transcribe

import (
	"context"
	"io"
	"sync"
	"time"

	gwstatus "github.com/sjawhar/ghost-wispr/internal/status"
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
		return gwstatus.ComponentStatusConnected
	case StateDisconnected:
		return gwstatus.ComponentStatusDisconnected
	case StateReconnecting:
		return gwstatus.ComponentStatusReconnecting
	case StateDraining:
		return "draining"
	default:
		return "unknown"
	}
}

// ResilientConfig holds configuration for the ResilientClient.
type ResilientConfig struct {
	// BufferSize is the ring buffer capacity in bytes. Zero holds
	// DefaultBufferSeconds of 16-bit mono audio at SampleRate.
	BufferSize int

	// SampleRate is the rate of the audio written, which sizes the default
	// buffer and times the stream (CapturedSince). Zero is 16000 Hz.
	SampleRate int

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

// StateChange is one change of a ResilientClient's state. Seq numbers the
// changes of every client in the process, in the order they happened, so a
// listener that may hear of a change late can tell it from a newer one: a
// listener that shows the current state applies a change only when no newer
// one has been applied (ResilientClient.CurrentState).
type StateChange struct {
	Seq   uint64
	State ConnectionState
}

// ConnectionCallback hears of each change of the client's state. ctx is
// cancelled when the client closes; a callback that waits must give up then,
// since Close waits for it only a bounded time (stateCallbackStopWait).
type ConnectionCallback func(ctx context.Context, change StateChange)

// BatchResult is a recording's batch transcription: its transcript, the
// model the provider reported, and its words with their times when the
// provider gives them. Refinement rebuilds the transcript from Words to drop
// the words "Delete text" removed.
type BatchResult struct {
	Transcript string
	Model      string
	Words      []TimestampedWord
}

type BatchTranscriber interface {
	Transcribe(ctx context.Context, audioPath string) (BatchResult, error)
}

// TimestampedWord is a word in a batch transcription with its recording span
// in seconds and the diarized speaker Deepgram assigned to it.
type TimestampedWord struct {
	Text       string
	Start, End float64
	Speaker    int
}

// ModelSelector is a BatchTranscriber that can transcribe with another
// model. A refinement of a recording with "Delete text" spans uses the model
// the spans were recorded under.
type ModelSelector interface {
	WithModel(model string) BatchTranscriber
}

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

	// OnStateChange is an optional callback for state transitions. It gets
	// every change in the order the changes happened, numbered, one at a
	// time, on a goroutine that holds no lock of the client's
	// (notifyStateChanges), so a slow callback delays later callbacks, never
	// the audio. When it falls maxQueuedStateChanges behind, the oldest
	// queued changes are dropped and the newest kept; Seq shows the gap.
	OnStateChange ConnectionCallback

	// OnStateChangeLocked, when set, is called at each change with Mu held,
	// before the change is queued for OnStateChange: for state that must
	// change at the same moment as the client's own, such as a warning that
	// says it is not connected. It must be quick and must not call the client.
	OnStateChangeLocked func(StateChange)

	// ctx is the application context; cancellation stops the reconnect loop.
	ctx context.Context
	// cancel cancels ctx, stopping the reconnect goroutine on Close().
	cancel context.CancelFunc
	// log is a printf-style logger.
	log func(string, ...any)

	// reconnectDelay tracks the current exponential backoff delay.
	reconnectDelay time.Duration

	// drainMu is held across each chunk drainBuffer reads and writes, and by
	// Finalize, so a finalize never overtakes buffered audio already in flight.
	drainMu sync.Mutex

	// accepted counts the bytes Write has taken, and origin is the count at
	// the first byte the current Client was sent: the stream Deepgram times
	// its results in starts there (CapturedSince). Both are guarded by Mu.
	accepted int64
	origin   int64
	// queuedAhead reports the bytes an AsyncSender in front of the client
	// holds (watchSendQueue), nil without one; drops the audio it dropped that
	// a word on the current socket can still come before (sendQueueDropped);
	// and forgottenDrops the bytes of the drops before the current socket's
	// stream, which Captured still counts. Guarded by Mu.
	queuedAhead    func() int64
	drops          []streamDrop
	forgottenDrops int64
	// stateSeq numbers the changes of State (StateChange.Seq). stateChanges
	// are the changes not yet handed to OnStateChange, oldest first;
	// notifying says a goroutine is handing them over, and notifierDone is
	// closed when that goroutine ends. All guarded by Mu.
	stateSeq     uint64
	stateChanges []StateChange
	notifying    bool
	notifierDone chan struct{}
}
