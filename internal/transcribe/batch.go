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

type deepgramBatchWord struct {
	Word           string  `json:"word"`
	PunctuatedWord string  `json:"punctuated_word"`
	Start          float64 `json:"start"`
	End            float64 `json:"end"`
}

type batchTranscriptionResult struct {
	transcript string
	words      []deepgramBatchWord
	models     []string
	modelInfo  map[string]deepgramModelInfo
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

// Transcribe returns the recording's transcript, its words with their times,
// and the one transcription model the Deepgram response reports.
func (d *deepgramBatchTranscriber) Transcribe(ctx context.Context, audioPath string) (BatchResult, error) {
	result, err := d.transcribe(ctx, audioPath)
	if err != nil {
		return BatchResult{}, err
	}
	model, err := deepgramResponseModel(result.models, result.modelInfo)
	if err != nil {
		return BatchResult{}, err
	}
	return BatchResult{Transcript: result.transcript, Model: model, Words: timestampedWords(result.words)}, nil
}

func (d *deepgramBatchTranscriber) Words(ctx context.Context, audioPath string) ([]TimestampedWord, error) {
	result, err := d.transcribe(ctx, audioPath)
	if err != nil {
		return nil, err
	}
	return timestampedWords(result.words), nil
}

// timestampedWords is Deepgram's words with their times, each as it is
// punctuated in the transcript, or as heard when Deepgram gives no punctuated
// form.
func timestampedWords(batchWords []deepgramBatchWord) []TimestampedWord {
	words := make([]TimestampedWord, len(batchWords))
	for i, word := range batchWords {
		text := word.PunctuatedWord
		if text == "" {
			text = word.Word
		}
		words[i] = TimestampedWord{Text: text, Start: word.Start, End: word.End}
	}
	return words
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
		Metadata struct {
			Models    []string                     `json:"models"`
			ModelInfo map[string]deepgramModelInfo `json:"model_info"`
		} `json:"metadata"`
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
		models:     decoded.Metadata.Models,
		modelInfo:  decoded.Metadata.ModelInfo,
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
