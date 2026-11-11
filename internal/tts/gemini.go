package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

const (
	vertexAIBaseURL      = "https://aiplatform.googleapis.com"
	geminiDefaultModel   = "gemini-3.8-flash-tts"
	geminiDefaultVoice   = "Kore"
	geminiVertexLocation = "global"
)

// GeminiProvider implements Provider using Gemini TTS through Vertex AI.
type GeminiProvider struct {
	project         string
	model           string
	defaultVoice    string
	authCredentials *auth.Credentials
	baseURL         string
	maxTextLen      int
	httpClient      *http.Client
}

// GeminiOption configures optional GeminiProvider settings.
type GeminiOption func(*GeminiProvider)

// WithGeminiBaseURL overrides the Vertex AI API base URL (useful for testing).
func WithGeminiBaseURL(url string) GeminiOption {
	return func(p *GeminiProvider) { p.baseURL = url }
}

// WithGeminiHTTPClient overrides the default HTTP client.
func WithGeminiHTTPClient(c *http.Client) GeminiOption {
	return func(p *GeminiProvider) { p.httpClient = c }
}

// WithGeminiMaxTextLength overrides the maximum text length.
func WithGeminiMaxTextLength(n int) GeminiOption {
	return func(p *GeminiProvider) { p.maxTextLen = n }
}

// WithGeminiAuthCredentials sets the Google credentials used to authorize Vertex AI requests.
func WithGeminiAuthCredentials(credentials *auth.Credentials) GeminiOption {
	return func(p *GeminiProvider) { p.authCredentials = credentials }
}

// NewGeminiProvider creates a Gemini TTS provider backed by Vertex AI's global endpoint.
func NewGeminiProvider(project, model, defaultVoice string, opts ...GeminiOption) (*GeminiProvider, error) {
	p := &GeminiProvider{
		project:      strings.TrimSpace(project),
		model:        strings.TrimSpace(model),
		defaultVoice: strings.TrimSpace(defaultVoice),
		baseURL:      vertexAIBaseURL,
		maxTextLen:   DefaultMaxTextLength,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
	if p.model == "" {
		p.model = geminiDefaultModel
	}
	if p.defaultVoice == "" {
		p.defaultVoice = geminiDefaultVoice
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.project == "" {
		return nil, fmt.Errorf("gemini tts: Vertex AI project is required")
	}
	if p.authCredentials == nil {
		return nil, fmt.Errorf("gemini tts: Google credentials are required")
	}

	return p, nil
}

func (p *GeminiProvider) Name() string { return "gemini" }

func (p *GeminiProvider) ReportedSpeech(req SpeechRequest) (model, voice string) {
	voice = strings.TrimSpace(req.Voice)
	if voice == "" {
		voice = p.defaultVoice
	}
	return p.model, voice
}

type geminiTTSRequest struct {
	Contents         []geminiTTSContent        `json:"contents"`
	GenerationConfig geminiTTSGenerationConfig `json:"generationConfig"`
}

type geminiTTSContent struct {
	Role  string          `json:"role"`
	Parts []geminiTTSPart `json:"parts"`
}

type geminiTTSPart struct {
	Text string `json:"text"`
}

type geminiTTSGenerationConfig struct {
	ResponseModalities []string              `json:"responseModalities"`
	SpeechConfig       geminiTTSSpeechConfig `json:"speechConfig"`
}

type geminiTTSSpeechConfig struct {
	VoiceConfig geminiTTSVoiceConfig `json:"voiceConfig"`
}

type geminiTTSVoiceConfig struct {
	PrebuiltVoiceConfig geminiTTSPrebuiltVoiceConfig `json:"prebuiltVoiceConfig"`
}

type geminiTTSPrebuiltVoiceConfig struct {
	VoiceName string `json:"voiceName"`
}

type geminiTTSResponse struct {
	Candidates   []geminiTTSCandidate `json:"candidates"`
	ModelVersion string               `json:"modelVersion"`
}

type geminiTTSCandidate struct {
	Content geminiTTSContentResponse `json:"content"`
}

type geminiTTSContentResponse struct {
	Parts []geminiTTSResponsePart `json:"parts"`
}

type geminiTTSResponsePart struct {
	InlineData geminiTTSInlineData `json:"inlineData"`
}

type geminiTTSInlineData struct {
	Data string `json:"data"`
}

type geminiTTSErrorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *GeminiProvider) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResponse, error) {
	if err := validateText(req.Text, p.maxTextLen); err != nil {
		return nil, fmt.Errorf("gemini tts: %w", err)
	}

	_, voice := p.ReportedSpeech(req)
	bodyJSON, err := json.Marshal(geminiTTSRequest{
		Contents: []geminiTTSContent{{
			Role:  "user",
			Parts: []geminiTTSPart{{Text: req.Text}},
		}},
		GenerationConfig: geminiTTSGenerationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig: geminiTTSSpeechConfig{
				VoiceConfig: geminiTTSVoiceConfig{PrebuiltVoiceConfig: geminiTTSPrebuiltVoiceConfig{VoiceName: voice}},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gemini tts: marshal request: %w", err)
	}

	endpoint := fmt.Sprintf(
		"%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		strings.TrimRight(p.baseURL, "/"),
		url.PathEscape(p.project),
		geminiVertexLocation,
		url.PathEscape(p.model),
	)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("gemini tts: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	token, err := p.authCredentials.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("gemini tts: get token: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token.Value)

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini tts: http request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	responseBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("gemini tts: read response body: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		var apiErr geminiTTSErrorResponse
		if json.Unmarshal(responseBody, &apiErr) == nil && apiErr.Error.Message != "" {
			return nil, fmt.Errorf("gemini tts: api error (status %d): %s", httpResp.StatusCode, apiErr.Error.Message)
		}
		return nil, fmt.Errorf("gemini tts: api error (status %d): %s", httpResp.StatusCode, string(responseBody))
	}

	var ttsResponse geminiTTSResponse
	if err := json.Unmarshal(responseBody, &ttsResponse); err != nil {
		return nil, fmt.Errorf("gemini tts: unmarshal response: %w", err)
	}
	model := strings.TrimSpace(ttsResponse.ModelVersion)
	if model == "" {
		model = "not reported by provider"
	}
	for _, candidate := range ttsResponse.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.InlineData.Data == "" {
				continue
			}
			wav, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return nil, fmt.Errorf("gemini tts: decode audio content: %w", err)
			}
			pcm, format, err := decodeGeminiWAV(wav)
			if err != nil {
				return nil, fmt.Errorf("gemini tts: decode WAV audio: %w", err)
			}
			return &SpeechResponse{Audio: pcm, Format: format, Model: model, Voice: voice}, nil
		}
	}

	return nil, fmt.Errorf("gemini tts: response contains no audio")
}

// decodeGeminiWAV extracts PCM16 samples from Gemini's unary WAV response.
func decodeGeminiWAV(wav []byte) ([]byte, audio.AudioFormat, error) {
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, audio.AudioFormat{}, fmt.Errorf("expected RIFF/WAVE data")
	}
	riffEnd := int(binary.LittleEndian.Uint32(wav[4:8])) + 8
	if riffEnd > len(wav) {
		return nil, audio.AudioFormat{}, fmt.Errorf("truncated RIFF container")
	}

	var (
		format    audio.AudioFormat
		hasFormat bool
		pcm       []byte
		hasPCM    bool
	)
	for offset := 12; offset < riffEnd; {
		if offset+8 > riffEnd {
			return nil, audio.AudioFormat{}, fmt.Errorf("truncated WAV chunk header")
		}
		chunkSize := int(binary.LittleEndian.Uint32(wav[offset+4 : offset+8]))
		chunkStart := offset + 8
		chunkEnd := chunkStart + chunkSize
		if chunkEnd > riffEnd {
			return nil, audio.AudioFormat{}, fmt.Errorf("truncated WAV chunk")
		}

		switch string(wav[offset : offset+4]) {
		case "fmt ":
			if hasFormat || chunkSize < 16 {
				return nil, audio.AudioFormat{}, fmt.Errorf("invalid WAV format chunk")
			}
			if encoding := binary.LittleEndian.Uint16(wav[chunkStart : chunkStart+2]); encoding != 1 {
				return nil, audio.AudioFormat{}, fmt.Errorf("unsupported WAV encoding %d; Gemini audio must be 16-bit PCM", encoding)
			}
			channels := int(binary.LittleEndian.Uint16(wav[chunkStart+2 : chunkStart+4]))
			sampleRate := int(binary.LittleEndian.Uint32(wav[chunkStart+4 : chunkStart+8]))
			bitDepth := binary.LittleEndian.Uint16(wav[chunkStart+14 : chunkStart+16])
			if channels <= 0 || sampleRate <= 0 || bitDepth != 16 {
				return nil, audio.AudioFormat{}, fmt.Errorf("unsupported WAV format: channels=%d sample_rate=%d bit_depth=%d", channels, sampleRate, bitDepth)
			}
			format = audio.AudioFormat{SampleRate: sampleRate, Channels: channels, Encoding: audio.EncodingPCM16}
			hasFormat = true
		case "data":
			if hasPCM {
				return nil, audio.AudioFormat{}, fmt.Errorf("multiple WAV data chunks are unsupported")
			}
			pcm = wav[chunkStart:chunkEnd]
			hasPCM = true
		}

		offset = chunkEnd
		if chunkSize%2 != 0 {
			offset++
			if offset > riffEnd {
				return nil, audio.AudioFormat{}, fmt.Errorf("truncated WAV padding")
			}
		}
	}
	if !hasFormat {
		return nil, audio.AudioFormat{}, fmt.Errorf("missing WAV format chunk")
	}
	if !hasPCM {
		return nil, audio.AudioFormat{}, fmt.Errorf("missing WAV data chunk")
	}
	if len(pcm)%2 != 0 {
		return nil, audio.AudioFormat{}, fmt.Errorf("WAV PCM data has an odd byte length")
	}
	return pcm, format, nil
}
