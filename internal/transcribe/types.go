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

// GracefulCloser is a client that can end its stream gracefully: it tells the
// peer the stream ends, so the peer processes the audio it has and returns its
// final results, and closes once the peer has closed or wait has passed. A
// client that is not one is closed at once.
type GracefulCloser interface {
	CloseGracefully(wait time.Duration) error
}

// ResultRelay is a socket that hands its results on through relay, which the
// client gives it once the socket is dialled: relay runs handOn, the handing
// on of one result, with CapturedSince and CapturedBefore timing points of
// that socket's own stream. A socket that retired, replaced though healthy,
// returns its last results while the next socket is live (retire), and the
// words in them are times into its own stream, not its successor's.
type ResultRelay interface {
	RelayResultsThrough(relay func(handOn func()))
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
	// pending is the chunk the drain took out of Buffer and has not yet sent
	// whole: what a failed or partial write left unsent. It is older than
	// everything in Buffer, so the next drain sends it first. The audio kept,
	// pending and Buffer together, is bounded by Buffer's size and is one tail
	// of the capture (bufferLocked). Guarded by Mu, and refilled only with
	// drainMu held.
	pending []byte
	// The current socket's stream during a drain, all guarded by Mu: took is
	// the audio it took; inFlight says a drain write is under way, and
	// droppedInFlight counts the kept audio an overflow dropped meanwhile; gap
	// says an overflow dropped audio the socket had not taken, after audio it
	// had, so its stream would jump and it ends at its next chunk
	// (errStreamGap). Only an overflow sets it: audio a replaced socket
	// refused goes back to the buffer's end, as the replaced socket's own.
	took            int64
	inFlight        bool
	droppedInFlight int64
	gap             bool

	// accepted counts the bytes Write has taken, and origin is the count at
	// the first byte the current Client was sent: the stream Deepgram times
	// its results in starts there (CapturedSince). Both are guarded by Mu.
	accepted int64
	origin   int64
	// current is the stream of the current Client, until it retires, and
	// relaying the stream of the socket whose result is being handed on
	// (relayFor), if any: CapturedSince and CapturedBefore time points of
	// relaying's stream while it is set. relayMu hands on one result at a
	// time, so relaying names one socket. retired are the sockets that
	// retired and may still return their last results (retire). Guarded by
	// Mu, but relayMu.
	current  *socketStream
	relaying *socketStream
	retired  []*retiredSocket
	relayMu  sync.Mutex
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
	// owning says a reconnect owner runs: the one goroutine that dials,
	// drains and rotates sockets, from its start until it leaves the client
	// connected (drainChunk). There is at most one. optionsGen counts the
	// changes of the factory's options (OptionsChanged), and dialedGen is
	// optionsGen when the current socket's dial began: the owner redials once
	// when they differ. All guarded by Mu.
	owning     bool
	optionsGen uint64
	dialedGen  uint64
}
