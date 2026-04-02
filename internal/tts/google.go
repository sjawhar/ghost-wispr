package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

const (
	googleTTSBaseURL   = "https://texttospeech.googleapis.com"
	googleDefaultVoice = "en-US-Neural2-F"
	googleDefaultLang  = "en-US"
)

// GoogleProvider implements Provider using the Google Cloud Text-to-Speech API.
type GoogleProvider struct {
	apiKey       string
	credPath     string
	tokenSource  func(ctx context.Context) (string, error)
	defaultVoice string
	defaultLang  string
	baseURL      string
	maxTextLen   int
	httpClient   *http.Client
}

// GoogleOption configures optional GoogleProvider settings.
type GoogleOption func(*GoogleProvider)

// WithGoogleBaseURL overrides the API base URL (useful for testing).
func WithGoogleBaseURL(url string) GoogleOption {
	return func(p *GoogleProvider) { p.baseURL = url }
}

// WithGoogleHTTPClient overrides the default HTTP client.
func WithGoogleHTTPClient(c *http.Client) GoogleOption {
	return func(p *GoogleProvider) { p.httpClient = c }
}

// WithGoogleMaxTextLength overrides the maximum text length.
func WithGoogleMaxTextLength(n int) GoogleOption {
	return func(p *GoogleProvider) { p.maxTextLen = n }
}

// WithGoogleCredentialPath sets the path to a Google service account JSON file.
func WithGoogleCredentialPath(path string) GoogleOption {
	return func(p *GoogleProvider) { p.credPath = path }
}

// NewGoogleProvider creates a Google Cloud TTS provider.
// It accepts EITHER an API key OR a service account credential file path.
// If credPath is provided (and apiKey is empty), it uses OAuth2 service account auth.
// If apiKey is provided, it uses API key auth.
func NewGoogleProvider(apiKey, defaultVoice string, opts ...GoogleOption) (*GoogleProvider, error) {
	p := &GoogleProvider{
		apiKey:       apiKey,
		defaultVoice: defaultVoice,
		defaultLang:  googleDefaultLang,
		baseURL:      googleTTSBaseURL,
		maxTextLen:   DefaultMaxTextLength,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
	if p.defaultVoice == "" {
		p.defaultVoice = googleDefaultVoice
	}
	for _, opt := range opts {
		opt(p)
	}

	// If no API key, try service account credentials
	if p.apiKey == "" && p.credPath != "" {
		creds, err := os.ReadFile(p.credPath)
		if err != nil {
			return nil, fmt.Errorf("google tts: read credentials: %w", err)
		}
		cfg, err := google.CredentialsFromJSONWithTypeAndParams(
			context.Background(), creds, google.ServiceAccount,
			google.CredentialsParams{Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}},
		)
		if err != nil {
			return nil, fmt.Errorf("google tts: parse credentials: %w", err)
		}
		p.tokenSource = func(ctx context.Context) (string, error) {
			tok, err := cfg.TokenSource.Token()
			if err != nil {
				return "", fmt.Errorf("google tts: get token: %w", err)
			}
			return tok.AccessToken, nil
		}
	} else if p.apiKey == "" {
		return nil, fmt.Errorf("google tts: either api key or credential path is required")
	}

	return p, nil
}

func (p *GoogleProvider) Name() string { return "google" }

// googleTTSRequest matches the Cloud TTS REST API request body.
type googleTTSRequest struct {
	Input       googleTTSInput       `json:"input"`
	Voice       googleTTSVoice       `json:"voice"`
	AudioConfig googleTTSAudioConfig `json:"audioConfig"`
}

type googleTTSInput struct {
	Text string `json:"text"`
}

type googleTTSVoice struct {
	LanguageCode string `json:"languageCode"`
	Name         string `json:"name"`
}

type googleTTSAudioConfig struct {
	AudioEncoding string `json:"audioEncoding"`
}

// googleTTSResponse matches the Cloud TTS REST API response.
type googleTTSResponse struct {
	AudioContent string `json:"audioContent"`
}

// googleTTSErrorResponse is the error shape returned by the API.
type googleTTSErrorResponse struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (p *GoogleProvider) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResponse, error) {
	if err := validateText(req.Text, p.maxTextLen); err != nil {
		return nil, fmt.Errorf("google tts: %w", err)
	}

	voice := req.Voice
	if voice == "" {
		voice = p.defaultVoice
	}
	lang := req.Language
	if lang == "" {
		lang = p.defaultLang
	}

	body := googleTTSRequest{
		Input: googleTTSInput{Text: req.Text},
		Voice: googleTTSVoice{
			LanguageCode: lang,
			Name:         voice,
		},
		AudioConfig: googleTTSAudioConfig{
			AudioEncoding: "MP3",
		},
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("google tts: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/v1/text:synthesize", p.baseURL)

	respBody, err := p.doWithRetry(ctx, url, bodyJSON)
	if err != nil {
		return nil, err
	}

	var ttsResp googleTTSResponse
	if err := json.Unmarshal(respBody, &ttsResp); err != nil {
		return nil, fmt.Errorf("google tts: unmarshal response: %w", err)
	}
	if ttsResp.AudioContent == "" {
		return nil, fmt.Errorf("google tts: empty audioContent in response")
	}

	audioBytes, err := base64.StdEncoding.DecodeString(ttsResp.AudioContent)
	if err != nil {
		return nil, fmt.Errorf("google tts: decode audio content: %w", err)
	}

	return &SpeechResponse{
		Audio: audioBytes,
		Format: audio.AudioFormat{
			SampleRate: 24000,
			Channels:   1,
			Encoding:   audio.EncodingMP3,
		},
	}, nil
}

// doWithRetry executes the HTTP request, retrying once on 429.
func (p *GoogleProvider) doWithRetry(ctx context.Context, url string, bodyJSON []byte) ([]byte, error) {
	const maxAttempts = 2

	for attempt := range maxAttempts {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
		if err != nil {
			return nil, fmt.Errorf("google tts: create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if p.tokenSource != nil {
			token, err := p.tokenSource(ctx)
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("Authorization", "Bearer "+token)
		} else {
			httpReq.Header.Set("X-Goog-Api-Key", p.apiKey)
		}

		resp, err := p.httpClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("google tts: http request: %w", err)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxAttempts-1 {
			resp.Body.Close()
			backoff := time.Duration(math.Pow(2, float64(attempt+1))) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
				continue
			}
		}

		defer resp.Body.Close()

		respBodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("google tts: read response body: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			var apiErr googleTTSErrorResponse
			if json.Unmarshal(respBodyBytes, &apiErr) == nil && apiErr.Error.Message != "" {
				return nil, fmt.Errorf("google tts: api error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
			}
			return nil, fmt.Errorf("google tts: api error (status %d): %s", resp.StatusCode, string(respBodyBytes))
		}

		return respBodyBytes, nil
	}

	return nil, fmt.Errorf("google tts: exhausted retries")
}
