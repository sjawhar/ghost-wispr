package tts

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// googleTTSRate is the rate the engine asks Cloud Text-to-Speech for.
const googleTTSRate = 24000

// GoogleTTS synthesises speech with Google Cloud Text-to-Speech's
// text:synthesize, as LINEAR16 at 24 kHz. Its answer is one whole clip, a
// WAV, whose PCM the engine emits at once, without the WAV's header. Each
// Cloud Text-to-Speech voice is a model (en-US-Chirp3-HD-Charon): the engine
// speaks in voice, or the model when voice is "", in the language the
// voice's name begins with.
//
// GoogleTTS is a Connector: a Session warms its way to the provider as it
// opens (httpEngine.connect).
type GoogleTTS struct {
	httpEngine
	endpoint string
	voice    string
}

func newGoogleTTS(p *providers.Provider, model, voice string, o *options) *GoogleTTS {
	e := newHTTPEngine("google tts", p, o)
	return &GoogleTTS{httpEngine: e, endpoint: e.host + "/v1/text:synthesize", voice: cmp.Or(voice, model)}
}

// Name names the engine.
func (*GoogleTTS) Name() string { return "google-tts" }

// Voice is the voice the engine speaks in.
func (e *GoogleTTS) Voice() string { return e.voice }

// Model is the voice the engine asks for, since each Cloud Text-to-Speech
// voice is a model.
func (e *GoogleTTS) Model() string { return e.voice }

// Format is PCM16 mono at 24 kHz, as every request asks for.
func (*GoogleTTS) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: googleTTSRate, Channels: 1, Encoding: audio.EncodingPCM16}
}

// Connect warms the Session's way to the provider.
func (e *GoogleTTS) Connect(context.Context) (Conn, error) { return e.connect(e.Stream), nil }

type googleTTSRequest struct {
	Input struct {
		Text string `json:"text"`
	} `json:"input"`
	Voice struct {
		LanguageCode string `json:"languageCode"`
		Name         string `json:"name"`
	} `json:"voice"`
	AudioConfig struct {
		AudioEncoding   string `json:"audioEncoding"`
		SampleRateHertz int    `json:"sampleRateHertz"`
	} `json:"audioConfig"`
}

// googleLanguage is the language a Cloud Text-to-Speech voice's name begins
// with: en-GB of en-GB-Chirp3-HD-Charon. A name that begins with none is
// spoken as en-US.
func googleLanguage(voice string) string {
	if parts := strings.SplitN(voice, "-", 3); len(parts) == 3 {
		return parts[0] + "-" + parts[1]
	}
	return "en-US"
}

// Stream synthesises text and emits its PCM once the clip has come.
func (e *GoogleTTS) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	var request googleTTSRequest
	request.Input.Text = text
	request.Voice.LanguageCode, request.Voice.Name = googleLanguage(e.voice), e.voice
	request.AudioConfig.AudioEncoding, request.AudioConfig.SampleRateHertz = "LINEAR16", googleTTSRate
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("google tts: marshal request: %w", err)
	}
	a, err := e.post(ctx, e.endpoint, body, googleCode)
	if err != nil {
		return err
	}
	defer a.close()
	// encoding/json decodes the base64 audioContent into its bytes, and
	// fails one that is not base64 with the offset of its first bad byte,
	// which quotes nothing of the answer.
	var clip struct {
		AudioContent []byte `json:"audioContent"`
	}
	if err := json.NewDecoder(a).Decode(&clip); err != nil {
		var corrupt base64.CorruptInputError
		if errors.As(err, &corrupt) {
			return fmt.Errorf("google tts: decode audioContent: %w", err)
		}
		return a.fail("read answer", err)
	}
	if len(clip.AudioContent) == 0 {
		return a.noAudio()
	}
	pcm, err := wavPCM(clip.AudioContent)
	if err != nil {
		return fmt.Errorf("google tts: %w", err)
	}
	if len(pcm) == 0 {
		return a.noAudio()
	}
	return a.emit(emit, pcm)
}

// wavPCM is the PCM of a WAV of PCM16 mono at googleTTSRate, without its
// header.
func wavPCM(wav []byte) ([]byte, error) {
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, errors.New("the answer's audio is not a WAV")
	}
	end := int(binary.LittleEndian.Uint32(wav[4:8])) + 8
	if end > len(wav) {
		return nil, errors.New("the answer's WAV is cut short")
	}
	var format bool
	for offset := 12; offset+8 <= end; {
		size := int(binary.LittleEndian.Uint32(wav[offset+4 : offset+8]))
		start := offset + 8
		if start+size > end {
			return nil, errors.New("the answer's WAV has a chunk cut short")
		}
		chunk := wav[start : start+size]
		switch string(wav[offset : offset+4]) {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("the answer's WAV has a short format chunk")
			}
			encoding, channels := binary.LittleEndian.Uint16(chunk[0:2]), binary.LittleEndian.Uint16(chunk[2:4])
			rate, bits := binary.LittleEndian.Uint32(chunk[4:8]), binary.LittleEndian.Uint16(chunk[14:16])
			if encoding != 1 || channels != 1 || rate != googleTTSRate || bits != 16 {
				return nil, fmt.Errorf("the answer's WAV is encoding %d, %d channels, %d Hz, %d bits; want PCM16 mono at %d Hz", encoding, channels, rate, bits, googleTTSRate)
			}
			format = true
		case "data":
			if !format {
				return nil, errors.New("the answer's WAV has data before its format")
			}
			return chunk, nil
		}
		offset = start + size + size%2 // a chunk of odd size is padded
	}
	return nil, errors.New("the answer's WAV has no data")
}
