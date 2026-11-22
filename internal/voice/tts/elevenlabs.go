package tts

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

const (
	// elevenLabsDefaultVoice is ElevenLabs' Rachel.
	elevenLabsDefaultVoice = "21m00Tcm4TlvDq8ikWAM"
	// elevenLabsRate is the rate the engine asks for: output_format
	// pcm_24000, headerless 16-bit little-endian samples, mono.
	elevenLabsRate = 24000
)

// ElevenLabs synthesises speech with ElevenLabs' streaming text-to-speech,
// /v1/text-to-speech/{voice}/stream, as PCM at 24 kHz, and emits each part
// as it arrives. The model is ElevenLabs' model_id, the voice its voice id.
//
// ElevenLabs is a Connector: a Session warms its way to the provider as it
// opens (httpEngine.connect).
type ElevenLabs struct {
	httpEngine
	endpoint string
	model    string
	voice    string
}

func newElevenLabs(p *providers.Provider, model, voice string, o *options) *ElevenLabs {
	e := &ElevenLabs{httpEngine: newHTTPEngine("elevenlabs tts", p, o), model: model, voice: cmp.Or(voice, elevenLabsDefaultVoice)}
	e.endpoint = fmt.Sprintf("%s/v1/text-to-speech/%s/stream?output_format=pcm_%d", e.host, url.PathEscape(e.voice), elevenLabsRate)
	return e
}

// Name names the engine.
func (*ElevenLabs) Name() string { return "elevenlabs" }

// Voice is the voice id the engine speaks in.
func (e *ElevenLabs) Voice() string { return e.voice }

// Model is the model_id the engine asks for.
func (e *ElevenLabs) Model() string { return e.model }

// Format is PCM16 mono at 24 kHz, as every request asks for.
func (*ElevenLabs) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: elevenLabsRate, Channels: 1, Encoding: audio.EncodingPCM16}
}

// Connect warms the Session's way to the provider.
func (e *ElevenLabs) Connect(context.Context) (Conn, error) { return e.connect(e.Stream), nil }

type elevenLabsSpeech struct {
	Text    string `json:"text"`
	ModelID string `json:"model_id"`
}

// Stream synthesises text and emits its PCM as it arrives.
func (e *ElevenLabs) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	body, err := json.Marshal(elevenLabsSpeech{Text: text, ModelID: e.model})
	if err != nil {
		return fmt.Errorf("elevenlabs tts: marshal request: %w", err)
	}
	a, err := e.post(ctx, e.endpoint, body, elevenLabsCode)
	if err != nil {
		return err
	}
	defer a.close()
	return a.streamPCM(emit)
}

// elevenLabsCode is the code of an ElevenLabs refusal: detail.status. A
// validation refusal's detail is a list, which names none.
func elevenLabsCode(body []byte) string {
	var refusal struct {
		Detail json.RawMessage `json:"detail"`
	}
	var detail struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &refusal) == nil {
		_ = json.Unmarshal(refusal.Detail, &detail)
	}
	return detail.Status
}
