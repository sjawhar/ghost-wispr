package tts

import (
	"context"
	"fmt"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// DefaultMaxTextLength is the maximum number of characters allowed in a
// single TTS request. Providers typically enforce their own limits, but
// we check locally to fail fast and avoid unnecessary network round-trips.
const DefaultMaxTextLength = 5000

// Provider is the interface implemented by all TTS backends.
type Provider interface {
	// Synthesize converts text to speech and returns the raw audio bytes.
	Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResponse, error)

	// Name returns the provider identifier (e.g. "elevenlabs", "google").
	Name() string
}

// SpeechRequest carries the parameters for a single synthesis call.
type SpeechRequest struct {
	Text     string
	Voice    string
	Language string
}

// SpeechResponse carries the synthesised audio and its format metadata.
type SpeechResponse struct {
	Audio      []byte
	Format     audio.AudioFormat
	DurationMs int64
}

// ErrTextTooLong is returned when the input text exceeds maxTextLength.
type ErrTextTooLong struct {
	Length    int
	MaxLength int
}

func (e *ErrTextTooLong) Error() string {
	return fmt.Sprintf("text length %d exceeds maximum %d characters", e.Length, e.MaxLength)
}

// validateText checks common pre-conditions shared by all providers.
func validateText(text string, maxLen int) error {
	if text == "" {
		return fmt.Errorf("text must not be empty")
	}
	if len(text) > maxLen {
		return &ErrTextTooLong{Length: len(text), MaxLength: maxLen}
	}
	return nil
}
