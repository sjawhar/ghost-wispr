package session

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio/audiotest"
)

// TestMain runs the tests with every real audio device refused: a test of
// this package may not open a microphone or a speaker (audiotest).
func TestMain(m *testing.M) { audiotest.Main(m) }
