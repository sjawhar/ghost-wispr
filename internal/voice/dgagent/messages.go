package dgagent

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// serverMessage is any JSON message Deepgram sends
// (/docs/voice-agent-outputs); each type reads its own fields.
type serverMessage struct {
	Type string `json:"type"`

	// ConversationText
	Role     string `json:"role"`
	Content  string `json:"content"`
	Behavior string `json:"conversational_behavior"` // "filler" for Think Live's

	// FunctionCallRequest and FunctionCallCancelled
	Functions []functionCall `json:"functions"`

	// Error and Warning
	Code        string `json:"code"`
	Description string `json:"description"`

	// LatencyReport: Deepgram sends the transcription's latency four times
	// a second, and each of the turn's other figures in a report of its own.
	STTLatency *float64 `json:"stt_latency"`
}

// functionCall is one function of a FunctionCallRequest or a
// FunctionCallCancelled.
type functionCall struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	ClientSide       bool   `json:"client_side"`
	ThoughtSignature string `json:"thought_signature"` // from some Gemini models, to send back unchanged
}

// err names the message's code, not its description: for some codes
// Deepgram quotes what the session sent, which can hold the room's words. A
// code that is no plain token is left out (plain).
func (m *serverMessage) err() error {
	return fmt.Errorf("deepgram %s %s", m.Type, plain(m.Code))
}

// plain is s, a code or a function's name Deepgram sent, when it is one plain
// token (providererr.Token), and "(not a plain word)" otherwise: a log
// record and an error keep nothing else Deepgram wrote.
func plain(s string) string {
	return cmp.Or(providererr.Token(s), "(not a plain word)")
}

// plainID is id, a function call's id Deepgram sent, when it is letters,
// digits and _ . - only, up to 64 of them, and "(not a plain id)" otherwise.
// An id can start with a digit, which a plain token cannot.
func plainID(id string) string {
	const idChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-"
	if id == "" || len(id) > 64 || strings.Trim(id, idChars) != "" {
		return "(not a plain id)"
	}
	return id
}

// injectAgentMessage speaks message word for word. The API reads the words
// from "message"; one without it closes the session
// (/docs/voice-agent-inject-agent-message).
type injectAgentMessage struct {
	Type     string `json:"type"`
	Message  string `json:"message"`
	Behavior string `json:"behavior"`
}

// injectUserMessage hands Deepgram text as if the room had said it
// (/docs/voice-agent-inject-user-message).
type injectUserMessage struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// functionCallResponse answers a client-side call
// (/docs/voice-agent-function-call-response), with the request's
// thought_signature when it had one (/docs/voice-agent-function-call-request).
type functionCallResponse struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	Name             string `json:"name"`
	Content          string `json:"content"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
}
