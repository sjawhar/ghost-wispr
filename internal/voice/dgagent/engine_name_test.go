package dgagent

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestVoiceCNamesItsEngine fails if voice C does not name itself as
// voice.engine does, for the question log.
func TestVoiceCNamesItsEngine(t *testing.T) {
	var v voice.Voice = &Agent{}
	named, ok := v.(voice.NamedEngine)
	if !ok || named.Engine() != config.VoiceEngineDGAgent {
		t.Fatalf("voice C names its engine %v (%t), want %q", named, ok, config.VoiceEngineDGAgent)
	}
}
