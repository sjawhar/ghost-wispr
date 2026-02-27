package summary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

type IdempotencyStore interface {
	ClaimSummaryRequest(sessionID, promptHash string) (bool, error)
}

type OpenAI struct {
	client *openai.Client
	model  string
	store  IdempotencyStore
	sleep  func(time.Duration)
}

func NewOpenAI(apiKey, model string, store IdempotencyStore) *OpenAI {
	config := openai.DefaultConfig(apiKey)
	return NewOpenAIWithConfig(config, model, store)
}

func NewOpenAIWithConfig(config openai.ClientConfig, model string, store IdempotencyStore) *OpenAI {
	if strings.TrimSpace(model) == "" {
		model = "gpt-4o-mini"
	}

	return &OpenAI{
		client: openai.NewClientWithConfig(config),
		model:  model,
		store:  store,
		sleep:  time.Sleep,
	}
}

func (s *OpenAI) Summarize(ctx context.Context, sessionID, transcript string) (string, error) {
	if len(strings.Fields(transcript)) < 20 {
		return "", nil
	}

	hash := sha256.Sum256([]byte(transcript))
	promptHash := hex.EncodeToString(hash[:])

	if s.store != nil {
		claimed, err := s.store.ClaimSummaryRequest(sessionID, promptHash)
		if err != nil {
			return "", fmt.Errorf("claim summary request: %w", err)
		}
		if !claimed {
			return "", nil
		}
	}

	req := openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: "Summarize the following office conversation transcript concisely in markdown. Include key topics, decisions made, and action items if any.",
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: transcript,
			},
		},
	}

	backoff := []time.Duration{1 * time.Second, 4 * time.Second, 16 * time.Second}
	var lastErr error
	for attempt := 0; attempt < len(backoff); attempt++ {
		resp, err := s.client.CreateChatCompletion(ctx, req)
		if err == nil {
			if len(resp.Choices) == 0 {
				return "", nil
			}
			return strings.TrimSpace(resp.Choices[0].Message.Content), nil
		}

		lastErr = err
		if attempt < len(backoff)-1 {
			s.sleep(backoff[attempt])
		}
	}

	return "", fmt.Errorf("openai summary failed after retries: %w", lastErr)
}
