package tts

import (
	"context"
	"fmt"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// DefaultMaxTextLength is the maximum number of characters allowed in a
// single TTS request when tts.max_length names none. Providers enforce their
// own limits too, but checking locally fails fast, before any request.
const DefaultMaxTextLength = 5000

// Provider is what the Orchestrator speaks through: EngineProvider, the tts
// role's model.
type Provider interface {
	// Synthesize converts text to speech and returns the whole speech.
	Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResponse, error)

	// Name names the provider in logs: the name the config gives it.
	Name() string
}

// Configurable is a Provider that can be off: cmd's speechSource speaks with
// the tts role saved last, which a settings change turns off (tts.model "").
// The Orchestrator counts a Provider that is not Configured as no provider,
// so POST /api/tts/speak answers 501 and queues nothing.
type Configurable interface {
	Configured() bool
}

// SpeechRequest carries the parameters for a single synthesis call. Voice
// "" is the provider's own.
type SpeechRequest struct {
	Text  string
	Voice string
}

// SpeechResponse carries the synthesised audio and its format, the model
// that spoke it, as its provider names it, and the voice it spoke in.
type SpeechResponse struct {
	Audio      []byte
	Format     audio.AudioFormat
	DurationMs int64
	Model      string
	Voice      string
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
