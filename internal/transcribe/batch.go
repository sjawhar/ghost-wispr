package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

type DeepgramBatchConfig struct {
	APIKey     string
	Model      string
	BaseURL    string
	Keywords   []string
	HTTPClient *http.Client
}

type deepgramBatchTranscriber struct {
	apiKey     string
	model      string
	baseURL    string
	keywords   []string
	httpClient *http.Client
}

func NewDeepgramBatchTranscriber(cfg *DeepgramBatchConfig) BatchTranscriber {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = "https://api.deepgram.com"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &deepgramBatchTranscriber{
		apiKey:     strings.TrimSpace(cfg.APIKey),
		model:      strings.TrimSpace(cfg.Model),
		baseURL:    strings.TrimRight(baseURL, "/"),
		keywords:   cfg.Keywords,
		httpClient: httpClient,
	}
}

func (d *deepgramBatchTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	audioBytes, err := os.ReadFile(audioPath)
	if err != nil {
		return "", fmt.Errorf("read audio file: %w", err)
	}

	u, err := url.Parse(d.baseURL)
	if err != nil {
		return "", fmt.Errorf("parse deepgram base url: %w", err)
	}
	u.Path = path.Join(u.Path, "/v1/listen")

	q := u.Query()
	if d.model != "" {
		q.Set("model", d.model)
	}
	q.Set("smart_format", "true")
	q.Set("punctuate", "true")
	for _, kw := range d.keywords {
		q.Add("keywords", kw)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(audioBytes))
	if err != nil {
		return "", fmt.Errorf("create deepgram request: %w", err)
	}
	if d.apiKey != "" {
		req.Header.Set("Authorization", "Token "+d.apiKey)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call deepgram batch api: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read deepgram response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("deepgram batch api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("decode deepgram response: %w", err)
	}
	if len(decoded.Results.Channels) == 0 || len(decoded.Results.Channels[0].Alternatives) == 0 {
		return "", fmt.Errorf("deepgram response missing transcript")
	}

	return strings.TrimSpace(decoded.Results.Channels[0].Alternatives[0].Transcript), nil
}
