package tts

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestVoiceANamesItsEngine fails if voice A does not name itself as
// voice.engine does, for the question log.
func TestVoiceANamesItsEngine(t *testing.T) {
	named, ok := New(nil).(voice.NamedEngine)
	if !ok || named.Engine() != config.VoiceEngineTTS {
		t.Fatalf("voice A names its engine %v (%t), want %q", named, ok, config.VoiceEngineTTS)
	}
}
