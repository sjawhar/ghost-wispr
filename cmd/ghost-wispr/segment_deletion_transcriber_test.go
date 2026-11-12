package main

import (
	"errors"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

// TestSegmentDeletionWithoutDeepgramIsNotConfigured fails if a segment
// deletion with no Deepgram API key returns an error the API cannot tell from
// a failed deletion: the API answers that one 500 with the developer's
// message, where it should say batch transcription is not configured (503).
func TestSegmentDeletionWithoutDeepgramIsNotConfigured(t *testing.T) {
	cfg := config.Config{BatchTranscription: config.BatchTranscription{Provider: "deepgram", Model: "nova-3"}}
	if _, err := segmentDeletionTranscriber(&cfg); !errors.Is(err, server.ErrBatchTranscriptionNotConfigured) {
		t.Fatalf("segment deletion transcriber without a Deepgram API key: error %v, want %v", err, server.ErrBatchTranscriptionNotConfigured)
	}

	// Control: with a key, the deletion has its transcriber.
	cfg.DeepgramAPIKey = "not-a-secret"
	if transcriber, err := segmentDeletionTranscriber(&cfg); err != nil || transcriber == nil {
		t.Fatalf("segment deletion transcriber with a Deepgram API key = (%v, %v), want a transcriber", transcriber, err)
	}
}
