package summary

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// A provider's error keeps no message, so the summary reads whether the
// transcript was too long for the model from the error's TooLong, and still
// summarizes it in parts; another error says so in its text.
func TestATooLongTranscriptIsSeenWithoutTheProvidersMessage(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("gemini json completion: %w", providererr.New("gemini", 400, "INVALID_ARGUMENT", "The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).")), true},
		{fmt.Errorf("openai completion: %w", providererr.New("openai", 400, "context_length_exceeded", "")), true},
		{fmt.Errorf("gemini completion: %w", providererr.New("gemini", 400, "INVALID_ARGUMENT", "Invalid content")), false},
		{errors.New("maximum context length exceeded"), true},
		{errors.New("connection reset"), false},
	} {
		if got := isContextOverflow(tc.err); got != tc.want {
			t.Errorf("isContextOverflow(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
