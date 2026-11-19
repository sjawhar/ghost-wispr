// Package voice runs Wispr's spoken exchanges in a meeting: a person says the
// wake phrase, a voice opens, the Concierge answers, and the Supervisor stops
// it all on a pause, a light hold, the Jabra button, shutdown or barge-in.
//
// These types are the contract every voice engine builds on: voice A
// (internal/voice/tts, our own streaming speech), voice B (internal/voice/gptlive)
// and voice C (internal/voice/dgagent).
package voice

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Exchange is one conversation with Wispr, from the wake phrase to its end.
type Exchange struct {
	SessionID string
	WakeAt    time.Time
	AskerTurn string             // the asker's words after the wake phrase, so far
	Recent    []TurnText         // up to the last 2 minutes of labelled final segments
	Tap       <-chan audio.Frame // room audio from the wake on, 16 kHz mono, only while the capture gate is open
	Out       Output             // the speaker output's "voice" input
	Tools     ToolRunner         // the Concierge
}

// TurnText is one final transcript segment.
type TurnText struct {
	Speaker int // Deepgram's speaker label; -1 when it gave none
	Text    string
	At      time.Time // when the segment arrived
}

// ToolCall is a voice's request to run one of the Concierge's tools.
type ToolCall struct {
	ID, Name string
	Args     json.RawMessage
}

// ToolRunner runs the Concierge's tools for a voice that drives its own turn
// (voices B and C). The Concierge's Run returns an error for an unknown tool
// or a stop; when a tool finds Dispatch or Envoy out of reach, it returns the
// tool's result, which says so, with an error that wraps ErrUnreachable. The
// Supervisor's runner, the one a voice gets as Exchange.Tools, plays the error
// sound then, and gives the voice the result with a nil error.
type ToolRunner interface {
	Run(ctx context.Context, c ToolCall) (json.RawMessage, error)
}

// AnswerTool is the tool a voice calls when its vendor hands the whole turn
// to us, as GPT-Live's client delegation does: "answer the asker". Its Args
// are AnswerArgs and its result an AnswerResult. x.Tools runs it through
// Concierge.Answer.
const AnswerTool = "answer"

// AnswerArgs are AnswerTool's arguments.
type AnswerArgs struct {
	Question   string `json:"question"`
	Transcript string `json:"transcript,omitempty"` // what the voice heard, labelled by speaker
}

// AnswerResult is AnswerTool's result, and the shape Deliver takes for a
// holder's late answer.
type AnswerResult struct {
	Say     string `json:"say"`               // for the ear
	Context string `json:"context,omitempty"` // silent facts for the voice's model
}

// Voice opens a Session for an Exchange. Each call gets an Exchange of its own,
// which the Supervisor does not change afterwards. ctx is the exchange's: a
// Session may end when ctx ends, after Open has returned too, so a caller must
// not pass a context that ends with the call alone.
type Voice interface {
	Open(ctx context.Context, x *Exchange) (Session, error)
}

// SayMode says what a spoken text is, for the voice's style and the log.
type SayMode int

const (
	Ack    SayMode = iota + 1 // an acknowledgement or a routing line
	Fact                      // a fact from the record
	Answer                    // a holder's answer
)

func (m SayMode) String() string {
	switch m {
	case Ack:
		return "ack"
	case Fact:
		return "fact"
	case Answer:
		return "answer"
	default:
		return "unknown"
	}
}

// Session is one open voice.
type Session interface {
	// Say speaks text. It returns no sooner than its speech is queued to the
	// Output (voice A returns then; a vendor voice may return once it has
	// played), and an error means the text was not heard: the Supervisor
	// keeps a held answer whose Say failed for the next invitation.
	Say(ctx context.Context, text string, mode SayMode) error
	// Deliver hands a late tool result to the voice, such as GPT-Live's
	// delegation id.
	Deliver(ctx context.Context, callID string, result json.RawMessage) error
	// Interrupt stops speaking now; the Supervisor also cancels Out.
	Interrupt()
	// Events reports what the voice does. The channel closes after Close.
	Events() <-chan Event
	Close() error
}

// EventKind names a Session event.
type EventKind int

const (
	EventSpeechStarted EventKind = iota + 1
	EventSpeechEnded
	EventUserTurn  // Text holds the turn the vendor heard
	EventToolCall  // Call holds the request
	EventErr       // Err holds the failure
	EventAgentText // Text holds words the voice speaks or is about to, for the own-speech filter
)

// Event is one thing a Session reports.
type Event struct {
	Kind EventKind
	Text string
	Call ToolCall
	Err  error
}

// Output is the speaker output's "voice" input as a voice plays through it.
// The integrator adapts *audio.Output to it; a voice never cancels the input
// itself (Interrupt stops its writes, and the Supervisor cancels the input).
type Output interface {
	// Begin starts one speech.
	Begin() Playback
}

// Playback is one speech on the voice input. *audio.Playback implements it.
type Playback interface {
	// Write queues frame, PCM16 in format, of at most audio.MaxWriteFrame. It
	// waits while the input holds 200 ms, so a voice that synthesises faster
	// than real time is held back, never dropped.
	Write(ctx context.Context, frame []byte, format audio.AudioFormat) error
	// Drain waits until the speech has played, or was cancelled.
	Drain(ctx context.Context) error
}

// Concierge answers for a voice that leaves the turn to us (voice A): the
// Supervisor hands it the asker's turn once Deepgram marks the turn final,
// and speaks what it says. Voices B and C call its tools instead.
type Concierge interface {
	// Tools gives x's tools, for x.Tools: ask_agent's pointer names x's
	// session and span. x.SessionID names a session the store already has.
	Tools(x *Exchange) ToolRunner
	// Heard tells it the asker's turn so far, on each result after the wake,
	// so it can read the record while the asker speaks. It returns at once;
	// the read ends with ctx, the exchange's, which every stop cancels. x
	// carries the SessionID and WakeAt the later Tools and Answer calls carry.
	Heard(ctx context.Context, x *Exchange, words string)
	// Answer answers one turn of x. It speaks through say as it goes (an
	// acknowledgement, then the facts) and returns once the turn is done. The
	// Supervisor cancels ctx to stop its model and tool calls. When Dispatch or
	// Envoy cannot be reached, it says so and returns an error that wraps
	// ErrUnreachable; the Supervisor then plays the error sound.
	Answer(ctx context.Context, x *Exchange, turn string, say SayFunc) error
	// Answers reports each holder's answer once it lands. The Supervisor
	// plays the answer sound and speaks it on invitation.
	Answers() <-chan PendingAnswer
	// Spoken records that a pending answer was spoken.
	Spoken(ctx context.Context, taskID string) error
	// NeverMind cancels the newest working question of sessionID, on "Wispr,
	// never mind", and tells its holder so.
	NeverMind(ctx context.Context, sessionID string) error
}

// ErrUnreachable marks a Concierge answer that could not reach Dispatch or
// Envoy.
var ErrUnreachable = errors.New("the Concierge cannot reach Dispatch or Envoy")

// SayFunc speaks text in the open exchange.
type SayFunc func(ctx context.Context, text string, mode SayMode) error

// PendingAnswer is a holder's answer to a question an exchange sent.
type PendingAnswer struct {
	TaskID    string
	SessionID string
	IssueKey  string
	Text      string    // rewritten for the ear
	AskedAt   time.Time // when the question was sent; a room word or a stop since then makes it wait for "Wispr, go ahead"
}
