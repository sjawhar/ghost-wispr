package gptlive

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestVoiceBNamesItsEngine fails if voice B does not name itself as
// voice.engine does, for the question log.
func TestVoiceBNamesItsEngine(t *testing.T) {
	var v voice.Voice = &Voice{}
	named, ok := v.(voice.NamedEngine)
	if !ok || named.Engine() != config.VoiceEngineGPTLive {
		t.Fatalf("voice B names its engine %v (%t), want %q", named, ok, config.VoiceEngineGPTLive)
	}
}
