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
	HTTPClient *http.Client
}

type deepgramBatchTranscriber struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
}

type deepgramBatchWord struct {
	Word           string  `json:"word"`
	PunctuatedWord string  `json:"punctuated_word"`
	Start          float64 `json:"start"`
	End            float64 `json:"end"`
}

type batchTranscriptionResult struct {
	transcript string
	words      []deepgramBatchWord
}

func NewDeepgramBatchTranscriber(cfg DeepgramBatchConfig) BatchWordsTranscriber {
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
		httpClient: httpClient,
	}
}

func (d *deepgramBatchTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	result, err := d.transcribe(ctx, audioPath)
	if err != nil {
		return "", err
	}
	return result.transcript, nil
}

func (d *deepgramBatchTranscriber) Words(ctx context.Context, audioPath string) ([]TimestampedWord, error) {
	result, err := d.transcribe(ctx, audioPath)
	if err != nil {
		return nil, err
	}

	words := make([]TimestampedWord, len(result.words))
	for i, word := range result.words {
		text := word.PunctuatedWord
		if text == "" {
			text = word.Word
		}
		words[i] = TimestampedWord{Text: text, Start: word.Start, End: word.End}
	}
	return words, nil
}

func (d *deepgramBatchTranscriber) transcribe(ctx context.Context, audioPath string) (batchTranscriptionResult, error) {
	audioBytes, err := os.ReadFile(audioPath)
	if err != nil {
		return batchTranscriptionResult{}, fmt.Errorf("read audio file: %w", err)
	}

	u, err := url.Parse(d.baseURL)
	if err != nil {
		return batchTranscriptionResult{}, fmt.Errorf("parse deepgram base url: %w", err)
	}
	u.Path = path.Join(u.Path, "/v1/listen")

	q := u.Query()
	if d.model != "" {
		q.Set("model", d.model)
	}
	q.Set("smart_format", "true")
	q.Set("punctuate", "true")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(audioBytes))
	if err != nil {
		return batchTranscriptionResult{}, fmt.Errorf("create deepgram request: %w", err)
	}
	if d.apiKey != "" {
		req.Header.Set("Authorization", "Token "+d.apiKey)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return batchTranscriptionResult{}, fmt.Errorf("call deepgram batch api: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return batchTranscriptionResult{}, fmt.Errorf("read deepgram response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return batchTranscriptionResult{}, fmt.Errorf("deepgram batch api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string              `json:"transcript"`
					Words      []deepgramBatchWord `json:"words"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return batchTranscriptionResult{}, fmt.Errorf("decode deepgram response: %w", err)
	}
	if len(decoded.Results.Channels) == 0 || len(decoded.Results.Channels[0].Alternatives) == 0 {
		return batchTranscriptionResult{}, fmt.Errorf("deepgram response missing transcript")
	}

	alternative := decoded.Results.Channels[0].Alternatives[0]
	return batchTranscriptionResult{
		transcript: strings.TrimSpace(alternative.Transcript),
		words:      alternative.Words,
	}, nil
}
