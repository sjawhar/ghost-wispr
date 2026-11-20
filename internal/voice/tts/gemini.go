package tts

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

const (
	geminiDefaultVoice = "Kore"
	// geminiRate is the only rate Gemini TTS streams: headerless PCM16 mono.
	geminiRate = 24000
	// geminiMaxEvent bounds one server-sent event: far more than the
	// base64 audio of one chunk.
	geminiMaxEvent = 16 << 20
	// geminiUnknownModel is the model StreamModel returns for an answer
	// whose modelVersion is no plain token.
	geminiUnknownModel = "unknown"
)

// Gemini synthesises speech with Gemini TTS's streamGenerateContent with
// alt=sse, on Vertex AI or on Google's Gemini API, which streams headerless
// PCM16 at 24 kHz mono as base64 inline data, one server-sent event at a
// time. Each event is decoded once, straight into the audio it carries, and
// names the model that spoke in modelVersion (StreamModel).
//
// Gemini is a Connector: a Session warms its way to the provider as it
// opens, so the first Say pays neither the sign-in nor the TLS dial.
type Gemini struct {
	httpEngine
	endpoint string
	model    string
	voice    string
}

// newVertex is Gemini TTS on a Vertex AI provider: the Google publisher
// model in the provider's project and location ("global" when it names
// none), at the location's host or the provider's url.
func newVertex(p *providers.Provider, model, voice string, o *options) (Voiced, error) {
	if p.Project == "" {
		return nil, fmt.Errorf("gemini tts: Vertex AI provider %q names no project", p.Name)
	}
	g := &Gemini{httpEngine: newHTTPEngine("gemini tts", p, o), model: model, voice: cmp.Or(voice, geminiDefaultVoice)}
	g.endpoint = fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:streamGenerateContent?alt=sse",
		g.host, url.PathEscape(p.Project), url.PathEscape(cmp.Or(p.Location, config.DefaultVertexLocation)), url.PathEscape(model))
	return g, nil
}

// newGeminiAPI is Gemini TTS on Google's Gemini API, in the request shape
// Vertex AI takes.
func newGeminiAPI(p *providers.Provider, model, voice string, o *options) *Gemini {
	g := &Gemini{httpEngine: newHTTPEngine("gemini tts", p, o), model: model, voice: cmp.Or(voice, geminiDefaultVoice)}
	g.endpoint = fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", g.host, url.PathEscape(model))
	return g
}

// Name names the engine.
func (*Gemini) Name() string { return "gemini" }

// Voice is the voice the engine speaks in.
func (g *Gemini) Voice() string { return g.voice }

// Model is the model the engine asks for.
func (g *Gemini) Model() string { return g.model }

// Format is PCM16 mono at 24 kHz, the only output Gemini TTS streams.
func (*Gemini) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: geminiRate, Channels: 1, Encoding: audio.EncodingPCM16}
}

// Connect warms the Session's way to the provider (httpEngine.connect).
func (g *Gemini) Connect(context.Context) (Conn, error) { return g.connect(g.Stream), nil }

type geminiRequest struct {
	Contents         []geminiContent        `json:"contents"`
	GenerationConfig geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string           `json:"responseModalities"`
	SpeechConfig       geminiSpeechConfig `json:"speechConfig"`
}

type geminiSpeechConfig struct {
	VoiceConfig geminiVoiceConfig `json:"voiceConfig"`
}

// geminiVoiceConfig names the voice in voiceConfig.voice, the field of the
// Gemini 3.8 TTS models, which takes a prebuilt, library or designed voice.
type geminiVoiceConfig struct {
	Voice string `json:"voice"`
}

// geminiEvent is one server-sent event of the stream: audio, or an error.
// encoding/json decodes the base64 data into its bytes.
type geminiEvent struct {
	Candidates []struct {
		FinishReason string `json:"finishReason"`
		Content      struct {
			Parts []struct {
				InlineData struct {
					Data []byte `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	ModelVersion string `json:"modelVersion"`
	Error        *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// Stream synthesises text and emits each event's audio as it arrives
// (StreamModel).
func (g *Gemini) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	_, err := g.StreamModel(ctx, text, emit)
	return err
}

// StreamModel synthesises text, emits each event's audio as it arrives and
// returns the model the stream's events name in modelVersion, as
// ModelReporter says. It fails when no answer, or no byte of it, comes for
// stallTimeout.
func (g *Gemini) StreamModel(ctx context.Context, text string, emit func(pcm []byte) error) (string, error) {
	body, err := json.Marshal(geminiRequest{
		Contents: []geminiContent{{Role: "user", Parts: []geminiPart{{Text: text}}}},
		GenerationConfig: geminiGenerationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig:       geminiSpeechConfig{VoiceConfig: geminiVoiceConfig{Voice: g.voice}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("gemini tts: marshal request: %w", err)
	}
	a, err := g.post(ctx, g.endpoint, body, googleCode)
	if err != nil {
		return "", err
	}
	defer a.close()

	events := bufio.NewScanner(a)
	events.Buffer(make([]byte, 0, 64<<10), geminiMaxEvent)
	var data []byte
	var audioSeen bool
	var finishReason, model string
	for {
		more := events.Scan()
		line := events.Bytes()
		if more && len(line) > 0 {
			// An event's data lines; any other field (event, id, a
			// comment) carries nothing for us.
			if field, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, bytes.TrimPrefix(field, []byte(" "))...)
			}
			continue
		}
		// A blank line, or the end of the stream, ends an event.
		if len(data) > 0 {
			a.pause()
			reason, version, err := emitGeminiEvent(data, g.provider, func(pcm []byte) error {
				audioSeen = audioSeen || len(pcm) > 0
				return emit(pcm)
			})
			if err != nil {
				return "", err
			}
			if reason != "" {
				finishReason = reason
			}
			if version != "" {
				model = version
			}
			a.resume()
			data = data[:0]
		}
		if !more {
			break
		}
	}
	if err := events.Err(); err != nil {
		return "", a.fail("read stream", err)
	}
	if !audioSeen {
		if finishReason != "" {
			return "", fmt.Errorf("gemini tts: %w (finish reason: %s)", ErrNoAudio, cmp.Or(providererr.Token(finishReason), "unnamed"))
		}
		return "", fmt.Errorf("gemini tts: %w", ErrNoAudio)
	}
	if model != "" && providererr.Token(model) == "" {
		return geminiUnknownModel, nil
	}
	return model, nil
}

// emitGeminiEvent decodes one event of the stream, emits its audio and
// returns the server's finish reason and the model it names, when the event
// supplied them. An error event keeps the status and code of provider, who
// answered: its message can quote the text spoken.
func emitGeminiEvent(data []byte, provider string, emit func(pcm []byte) error) (string, string, error) {
	var event geminiEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return "", "", fmt.Errorf("gemini tts: decode event: %w", err)
	}
	if e := event.Error; e != nil {
		return "", "", fmt.Errorf("gemini tts: stream error: %w", providererr.New(provider, e.Code, e.Status, e.Message))
	}
	var finishReason string
	for _, candidate := range event.Candidates {
		if candidate.FinishReason != "" {
			finishReason = candidate.FinishReason
		}
		for _, part := range candidate.Content.Parts {
			if len(part.InlineData.Data) == 0 {
				continue
			}
			if err := emit(part.InlineData.Data); err != nil {
				return "", "", err
			}
		}
	}
	return finishReason, strings.TrimSpace(event.ModelVersion), nil
}
