package tts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

const (
	elevenLabsBaseURL      = "https://api.elevenlabs.io"
	elevenLabsDefaultVoice = "21m00Tcm4TlvDq8ikWAM" // Rachel
)

// ElevenLabsProvider implements Provider using the ElevenLabs TTS API.
type ElevenLabsProvider struct {
	apiKey       string
	defaultVoice string
	baseURL      string
	maxTextLen   int
	httpClient   *http.Client
}

// ElevenLabsOption configures optional ElevenLabsProvider settings.
type ElevenLabsOption func(*ElevenLabsProvider)

// WithElevenLabsBaseURL overrides the API base URL (useful for testing).
func WithElevenLabsBaseURL(url string) ElevenLabsOption {
	return func(p *ElevenLabsProvider) { p.baseURL = url }
}

// WithElevenLabsHTTPClient overrides the default HTTP client.
func WithElevenLabsHTTPClient(c *http.Client) ElevenLabsOption {
	return func(p *ElevenLabsProvider) { p.httpClient = c }
}

// WithElevenLabsMaxTextLength overrides the maximum text length.
func WithElevenLabsMaxTextLength(n int) ElevenLabsOption {
	return func(p *ElevenLabsProvider) { p.maxTextLen = n }
}

// NewElevenLabsProvider creates a new ElevenLabs TTS provider.
func NewElevenLabsProvider(apiKey, defaultVoice string, opts ...ElevenLabsOption) (*ElevenLabsProvider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("elevenlabs: api key is required")
	}

	p := &ElevenLabsProvider{
		apiKey:       apiKey,
		defaultVoice: defaultVoice,
		baseURL:      elevenLabsBaseURL,
		maxTextLen:   DefaultMaxTextLength,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
	if p.defaultVoice == "" {
		p.defaultVoice = elevenLabsDefaultVoice
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *ElevenLabsProvider) Name() string { return "elevenlabs" }

// elevenLabsRequest is the JSON body sent to the TTS endpoint.
type elevenLabsRequest struct {
	Text    string `json:"text"`
	ModelID string `json:"model_id"`
}

// elevenLabsErrorResponse is the error shape returned by the API.
type elevenLabsErrorResponse struct {
	Detail struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"detail"`
}

func (p *ElevenLabsProvider) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResponse, error) {
	if err := validateText(req.Text, p.maxTextLen); err != nil {
		return nil, fmt.Errorf("elevenlabs: %w", err)
	}

	voice := req.Voice
	if voice == "" {
		voice = p.defaultVoice
	}

	body := elevenLabsRequest{
		Text:    req.Text,
		ModelID: "eleven_multilingual_v2",
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/v1/text-to-speech/%s", p.baseURL, voice)

	audioBytes, err := p.doWithRetry(ctx, url, bodyJSON)
	if err != nil {
		return nil, err
	}

	return &SpeechResponse{
		Audio: audioBytes,
		Format: audio.AudioFormat{
			SampleRate: 44100,
			Channels:   1,
			Encoding:   audio.EncodingMP3,
		},
	}, nil
}

// doWithRetry executes the HTTP request, retrying once on 429.
func (p *ElevenLabsProvider) doWithRetry(ctx context.Context, url string, bodyJSON []byte) ([]byte, error) {
	const maxAttempts = 2

	for attempt := range maxAttempts {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(bodyJSON)))
		if err != nil {
			return nil, fmt.Errorf("elevenlabs: create request: %w", err)
		}
		httpReq.Header.Set("xi-api-key", p.apiKey)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "audio/mpeg")

		resp, err := p.httpClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("elevenlabs: http request: %w", err)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxAttempts-1 {
			_ = resp.Body.Close()
			backoff := time.Duration(math.Pow(2, float64(attempt+1))) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
				continue
			}
		}

		// Successful response or non-retryable error
		result, err := p.processResponse(resp)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		return result, nil
	}

	// Unreachable, but the compiler needs it.
	return nil, fmt.Errorf("elevenlabs: exhausted retries")
}

func (p *ElevenLabsProvider) processResponse(resp *http.Response) ([]byte, error) {
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		var apiErr elevenLabsErrorResponse
		if json.Unmarshal(errBody, &apiErr) == nil && apiErr.Detail.Message != "" {
			return nil, fmt.Errorf("elevenlabs: api error (status %d): %s", resp.StatusCode, apiErr.Detail.Message)
		}
		return nil, fmt.Errorf("elevenlabs: api error (status %d): %s", resp.StatusCode, string(errBody))
	}

	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: read response body: %w", err)
	}
	if len(audioBytes) == 0 {
		return nil, fmt.Errorf("elevenlabs: empty audio response")
	}

	return audioBytes, nil
}
