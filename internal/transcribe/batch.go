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
	"slices"
	"strings"
)

type deepgramModelInfo struct {
	Name string `json:"name"`
	Arch string `json:"arch"`
}

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

func NewDeepgramBatchTranscriber(cfg DeepgramBatchConfig) BatchTranscriber {
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

func (d *deepgramBatchTranscriber) Transcribe(ctx context.Context, audioPath string) (BatchResult, error) {
	audioBytes, err := os.ReadFile(audioPath)
	if err != nil {
		return BatchResult{}, fmt.Errorf("read audio file: %w", err)
	}

	u, err := url.Parse(d.baseURL)
	if err != nil {
		return BatchResult{}, fmt.Errorf("parse deepgram base url: %w", err)
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
		return BatchResult{}, fmt.Errorf("create deepgram request: %w", err)
	}
	if d.apiKey != "" {
		req.Header.Set("Authorization", "Token "+d.apiKey)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return BatchResult{}, fmt.Errorf("call deepgram batch api: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return BatchResult{}, fmt.Errorf("read deepgram response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return BatchResult{}, fmt.Errorf("deepgram batch api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded struct {
		Metadata struct {
			Models    []string                     `json:"models"`
			ModelInfo map[string]deepgramModelInfo `json:"model_info"`
		} `json:"metadata"`
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return BatchResult{}, fmt.Errorf("decode deepgram response: %w", err)
	}
	if len(decoded.Results.Channels) == 0 || len(decoded.Results.Channels[0].Alternatives) == 0 {
		return BatchResult{}, fmt.Errorf("deepgram response missing transcript")
	}
	model, err := deepgramResponseModel(decoded.Metadata.Models, decoded.Metadata.ModelInfo)
	if err != nil {
		return BatchResult{}, err
	}
	return BatchResult{
		Transcript: strings.TrimSpace(decoded.Results.Channels[0].Alternatives[0].Transcript),
		Model:      model,
	}, nil
}

func deepgramResponseModel(models []string, modelInfo map[string]deepgramModelInfo) (string, error) {
	if len(models) == 0 {
		return "", fmt.Errorf("deepgram response missing metadata.models")
	}
	names := make([]string, 0, len(models))
	for _, id := range models {
		info, ok := modelInfo[id]
		if !ok {
			return "", fmt.Errorf("deepgram response metadata.models references unknown model %q", id)
		}
		name := strings.TrimSpace(info.Arch)
		if name == "" {
			name = strings.TrimSpace(info.Name)
		}
		if name == "" {
			return "", fmt.Errorf("deepgram response metadata.model_info[%q] has no model identity", id)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) != 1 {
		return "", fmt.Errorf("deepgram response must report exactly one transcription model, got %q", names)
	}
	return names[0], nil
}
