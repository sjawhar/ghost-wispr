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

	"github.com/sjawhar/ghost-wispr/internal/providererr"
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
	Speaker        int     `json:"speaker"`
}

type batchTranscriptionResult struct {
	transcript string
	words      []deepgramBatchWord
	models     []string
	modelInfo  map[string]deepgramModelInfo
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

// WithModel returns the transcriber with model in place of its configured one.
func (d *deepgramBatchTranscriber) WithModel(model string) BatchTranscriber {
	copied := *d
	copied.model = strings.TrimSpace(model)
	return &copied
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
		words[i] = TimestampedWord{Text: text, Start: word.Start, End: word.End, Speaker: word.Speaker}
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
	q.Set("filler_words", "true")
	q.Set("diarize", "true")
	// Keeps the audio and its transcript out of Deepgram's Model Improvement
	// Program, which may otherwise keep them after processing, for training.
	q.Set("mip_opt_out", "true")
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
		return batchTranscriptionResult{}, fmt.Errorf("call deepgram batch api: %w", providererr.From("deepgram", err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return batchTranscriptionResult{}, fmt.Errorf("read deepgram response body: %w", providererr.From("deepgram", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return batchTranscriptionResult{}, fmt.Errorf("deepgram batch api: %w", providererr.Reduce("deepgram", resp.StatusCode, body, "err_code"))
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

// deepgramResponseModel is the one transcription model a Deepgram answer
// reports. Its errors name none of Deepgram's strings, and a model whose name
// is no plain token (providererr.Token) is refused: the name goes into
// errors and status warnings about a refinement's model.
func deepgramResponseModel(models []string, modelInfo map[string]deepgramModelInfo) (string, error) {
	if len(models) == 0 {
		return "", fmt.Errorf("deepgram response missing metadata.models")
	}
	names := make([]string, 0, len(models))
	for i, id := range models {
		info, ok := modelInfo[id]
		if !ok {
			return "", fmt.Errorf("deepgram response metadata.models[%d] names no model in metadata.model_info", i)
		}
		name := strings.TrimSpace(info.Arch)
		if name == "" {
			name = strings.TrimSpace(info.Name)
		}
		if name == "" {
			return "", fmt.Errorf("deepgram response metadata.model_info for metadata.models[%d] has no model identity", i)
		}
		if providererr.Token(name) == "" {
			return "", fmt.Errorf("deepgram response metadata.model_info for metadata.models[%d] names its model in no plain token", i)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) != 1 {
		return "", fmt.Errorf("deepgram response must report exactly one transcription model, got %d", len(names))
	}
	return names[0], nil
}
