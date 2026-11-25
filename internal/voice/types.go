// Package voice runs Wispr's spoken exchanges in a meeting: a person says the
// wake phrase, a voice opens, the Concierge answers, and the Supervisor stops
// it all on a pause, a light hold, the Jabra button, shutdown or barge-in.
//
// These types are the contract every voice engine builds on: voice A
// (internal/voice/tts, text-to-speech), voice B (internal/voice/gptlive,
// speech-to-speech) and voice C (internal/voice/dgagent, a voice agent).
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
	AskerTurn string // the asker's words after the wake phrase, so far
	// IssueKey is the canonical resolved Dispatch key for the answer this
	// exchange speaks, or empty when no issue was resolved. Held answers set
	// it directly; voices never reconstruct it from speech.
	IssueKey string
	// FollowUpTurn: AskerTurn was said in the follow-up window with no wake
	// phrase (a question; R10). The Concierge asks an agent about it only
	// when it says "ask".
	FollowUpTurn bool
	Recent       []TurnText         // up to the last 2 minutes of labelled final segments
	Tap          <-chan audio.Frame // room audio from the wake on, 16 kHz mono, only while the capture gate is open
	Out          Output             // the speaker output's "voice" input
	Tools        ToolRunner         // the Concierge
	// Turn numbers the asker's turns in the exchange: the Heard calls of a
	// turn and its Answer carry one number, and a later turn a greater one.
	// Once the Concierge begins to answer a turn, a Heard of that turn or an
	// earlier one, which the Supervisor makes apart from the answer and can
	// deliver after it, reads nothing.
	Turn uint64
	// Reply is, after Wispr asked the room a question, the room's own words
	// since that question ended: every final since then, in order, one a
	// line, through the turn answered (AskerTurn, Answer's turn). The
	// Concierge decides on it whether the reply is side talk, never on a
	// vendor's text, and answers from it when the turn holds no question of
	// its own. An endpoint inside the reply ("okay, ABC-12" … "can wait")
	// leaves the turn only its end. Empty when Wispr asked nothing.
	Reply string
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
	Say      string `json:"say"`                 // for the ear
	Context  string `json:"context,omitempty"`   // silent facts for the voice's model
	IssueKey string `json:"issue_key,omitempty"` // canonical resolved key; no speech parsing
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
	// Lead names the issue a search chose (R5, "I took KEY."): it leads the
	// answer and is never dropped, and it holds no fact to time.
	Lead
)

func (m SayMode) String() string {
	switch m {
	case Ack:
		return "ack"
	case Fact:
		return "fact"
	case Answer:
		return "answer"
	case Lead:
		return "lead"
	default:
		return "unknown"
	}
}

// Session is one open voice.
type Session interface {
	// Say speaks text. It returns no sooner than its speech is queued to the
	// Output (voice A returns then; a vendor voice may return once it has
	// played), and an error means the text was not heard: the Supervisor
	// keeps a held answer whose Say failed for the next invitation. ctx must
	// be the exchange's own context: voice A plays the text on under it after
	// Say returns, so a context that ends sooner cuts the text.
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
	// EventFactCheck reports how many issue keys, pull request numbers and
	// figures GPT-Live changed or dropped while speaking a written fact.
	// Count holds no words, so the room test can record it safely.
	EventFactCheck
	// EventTextAudio: Text, one Say's text, begins to play (its first frame
	// is queued to the Output). A voice that synthesises a Say while the one
	// before plays (voice A) reports it, so the first fact's audio is timed
	// right; a voice that does not report it is timed by its speech.
	EventTextAudio
)

// TextAudioReporter is a Session that reports EventTextAudio for each Say's
// text, as voice A does: its Say returns before the text plays.
type TextAudioReporter interface {
	ReportsTextAudio() bool
}

// SpeechWaiter is a Session whose Say returns before its text has played, as
// voice A's does. WaitSpoken returns nil once every Say called before it has
// played its last frame, else why one did not: an Interrupt, a failed
// synthesis or write, or a playback the Output ended.
type SpeechWaiter interface {
	WaitSpoken(ctx context.Context) error
}

// Event is one thing a Session reports.
type Event struct {
	Kind  EventKind
	Text  string
	Call  ToolCall
	Err   error
	Count int
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
	// ErrUnreachable; the Supervisor then plays the error sound. When the turn
	// is side talk, it says nothing and returns an error that wraps
	// ErrSideTalk; the Supervisor then closes the exchange.
	Answer(ctx context.Context, x *Exchange, turn string, say SayFunc) error
	// Answers reports each holder's answer once it lands. The Supervisor
	// plays the answer sound and speaks it on invitation.
	Answers() <-chan PendingAnswer
	// Spoken records that a pending answer was spoken.
	Spoken(ctx context.Context, taskID string) error
	// QuestionsAsked is the number of the last question the Concierge asked:
	// a mark, read when "Wispr, never mind" is said, that orders the never
	// mind among the questions whatever the wall clock does. It returns at
	// once.
	QuestionsAsked() uint64
	// NeverMind cancels the newest working question of sessionID, by number,
	// among those numbered at most asked (QuestionsAsked when "Wispr, never
	// mind" was said), and tells its holder so. cancelled reports that there
	// was one; with none, the Supervisor drops its newest held answer
	// instead.
	NeverMind(ctx context.Context, sessionID string, asked uint64) (cancelled bool, err error)
}

// WaitingLight shows the room that something waits for a person: a held
// answer waiting for "Wispr, go ahead" (the blink(1)'s LED 2, which flashes
// green while the unit records). On and Off must return at once.
type WaitingLight interface {
	On()
	Off()
}

// ErrUnreachable marks a Concierge answer that could not reach Dispatch or
// Envoy.
var ErrUnreachable = errors.New("the Concierge cannot reach Dispatch or Envoy")

// ErrSideTalk marks a turn the Concierge took for side talk: a reply to
// Wispr's own question ("Did you mean …?") that picks nothing it offered and
// asks no question. Nothing was said, searched or sent, and the exchange
// closes as side talk in the follow-up window does: no word, no sorry, no
// error sound (R10).
var ErrSideTalk = errors.New("the turn is side talk, not a question")

// SayFunc speaks text in the open exchange. Its ctx reaches the voice's Say
// (Session.Say), so it must be the exchange's own context, the one Answer
// was given, never one that ends when Answer returns.
type SayFunc func(ctx context.Context, text string, mode SayMode) error

// PendingAnswer is a holder's answer to a question an exchange sent.
type PendingAnswer struct {
	TaskID    string
	SessionID string
	IssueKey  string
	Text      string    // rewritten for the ear
	AskedAt   time.Time // when the question was sent; a room word or a stop since then makes it wait for "Wispr, go ahead"
}
