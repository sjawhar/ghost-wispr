package dgagent

import "fmt"

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
// Deepgram quotes what the session sent, which can hold the room's words.
func (m *serverMessage) err() error {
	return fmt.Errorf("deepgram %s %s", m.Type, m.Code)
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
