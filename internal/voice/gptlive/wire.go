package gptlive

import "encoding/json"

// The Live WebSocket's events this voice sends and reads, as the API
// reference names them (https://developers.openai.com/api/reference/resources/live/primary-websocket)
// and openai-go v3.103.0's live package types them. openai-go's Live
// WebSocket needs Go 1.26 and this module builds with Go 1.25, so this
// package owns the socket and these few shapes.

// Client events.
const (
	evSessionStart         = "session.start"
	evSessionClose         = "session.close"
	evInputAudioAppend     = "session.input_audio.append"
	evInstructionsAppend   = "session.instructions.append"
	evThinkingAppend       = "session.thinking.append"
	evCommentaryAppend     = "session.commentary.append"
	delegationTargetClient = "client"
)

// Server events.
const (
	evSessionStarted       = "session.started"
	evSessionClosed        = "session.closed"
	evError                = "error"
	evOutputAudioDelta     = "session.output_audio.delta"
	evInputTranscriptDelta = "session.input_transcript.delta"
	evOutputTranscript     = "session.output_transcript.delta"
	evDelegationCreated    = "session.delegation.created"
	evInstructionsAppended = "session.instructions.appended"
	evThinkingAppended     = "session.thinking.appended"
	evCommentaryAppended   = "session.commentary.appended"
)

// sessionStart is session.start: the session's whole configuration, which
// is immutable after startup.
type sessionStart struct {
	Type    string        `json:"type"`
	EventID string        `json:"event_id,omitempty"`
	Session sessionConfig `json:"session"`
}

type sessionConfig struct {
	Model        string        `json:"model"`
	Instructions string        `json:"instructions,omitempty"`
	Store        bool          `json:"store"`
	Delegation   delegation    `json:"delegation"`
	Audio        sessionAudio  `json:"audio"`
	Input        []initialItem `json:"input,omitempty"`
}

type delegation struct {
	Type string `json:"type"`
}

type sessionAudio struct {
	Format audioFormat  `json:"format"`
	Output *audioOutput `json:"output,omitempty"`
}

// audioFormat is audio/pcm: mono PCM16 little-endian, for both directions.
type audioFormat struct {
	Type string `json:"type"`
	Rate int    `json:"rate"`
}

type audioOutput struct {
	Voice string `json:"voice"`
}

// initialItem is one message of the text history a session starts with.
type initialItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Content []itemContent `json:"content"`
}

type itemContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// inputAudioAppend is session.input_audio.append. It has no acknowledgement.
type inputAudioAppend struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

// contextAppend is session.instructions.append, session.thinking.append or
// session.commentary.append. DelegationID is required and nullable: nil is
// session-wide context.
type contextAppend struct {
	Type         string  `json:"type"`
	EventID      string  `json:"event_id"`
	DelegationID *string `json:"delegation_id"`
	Content      string  `json:"content"`
}

type sessionClose struct {
	Type    string `json:"type"`
	EventID string `json:"event_id,omitempty"`
}

// serverEvent is every server event this voice reads, flattened: each event
// sets the fields its type has.
type serverEvent struct {
	Type          string          `json:"type"`
	EventID       string          `json:"event_id"`
	ClientEventID string          `json:"client_event_id"`
	Session       json.RawMessage `json:"session"`
	Delta         string          `json:"delta"`
	Delegation    struct {
		ID     string `json:"id"`
		Target string `json:"target"`
	} `json:"delegation"`
	Reason string   `json:"reason"`
	Error  apiError `json:"error"`
}

// apiError is a Live error event's error.
type apiError struct {
	Type          string `json:"type"`
	Code          string `json:"code"`
	Message       string `json:"message"`
	Param         string `json:"param"`
	ClientEventID string `json:"client_event_id"`
}
