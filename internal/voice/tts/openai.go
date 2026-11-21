package tts

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

const (
	// openAIDefaultVoice is the voice OpenAI recommends for the best
	// quality.
	openAIDefaultVoice = "marin"
	// openAIRate is the rate of the speech API's pcm format: headerless
	// 16-bit little-endian samples, mono.
	openAIRate = 24000
)

// OpenAI synthesises speech with the OpenAI API's /audio/speech, whose pcm
// answer streams by chunked transfer: each part of the PCM is emitted as it
// arrives. A model gateway that speaks the OpenAI API serves it the same way.
//
// OpenAI is a Connector: a Session warms its way to the provider as it
// opens (httpEngine.connect).
type OpenAI struct {
	httpEngine
	endpoint string
	model    string
	voice    string
}

func newOpenAI(p *providers.Provider, model, voice string, o *options) *OpenAI {
	e := newHTTPEngine("openai tts", p, o)
	return &OpenAI{httpEngine: e, endpoint: e.host + "/audio/speech", model: model, voice: cmp.Or(voice, openAIDefaultVoice)}
}

// Name names the engine.
func (*OpenAI) Name() string { return "openai" }

// Voice is the voice the engine speaks in.
func (e *OpenAI) Voice() string { return e.voice }

// Model is the model the engine asks for.
func (e *OpenAI) Model() string { return e.model }

// Format is PCM16 mono at 24 kHz, the speech API's pcm.
func (*OpenAI) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: openAIRate, Channels: 1, Encoding: audio.EncodingPCM16}
}

// Connect warms the Session's way to the provider.
func (e *OpenAI) Connect(context.Context) (Conn, error) { return e.connect(e.Stream), nil }

type openAISpeech struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	Voice          string `json:"voice"`
	ResponseFormat string `json:"response_format"`
}

// Stream synthesises text and emits its PCM as it arrives.
func (e *OpenAI) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	body, err := json.Marshal(openAISpeech{Model: e.model, Input: text, Voice: e.voice, ResponseFormat: "pcm"})
	if err != nil {
		return fmt.Errorf("openai tts: marshal request: %w", err)
	}
	a, err := e.post(ctx, e.endpoint, body, openAICode)
	if err != nil {
		return err
	}
	defer a.close()
	return a.streamPCM(emit)
}

// openAICode is the code of an OpenAI refusal: error.code, or error.type
// when the code is no token (OpenAI sends some codes as numbers, or null).
func openAICode(body []byte) string {
	var refusal struct {
		Error struct {
			Code any    `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &refusal)
	if code, _ := refusal.Error.Code.(string); providererr.Token(code) != "" {
		return code
	}
	return refusal.Error.Type
}
