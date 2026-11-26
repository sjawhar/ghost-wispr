package concierge

import (
	"os"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestMain runs the tests with every real audio device refused: a test of
// this package may not open a microphone or a speaker (audiotest.Main, which
// this is with the stores no rig took closed and removed once the tests end:
// freshStores).
func TestMain(m *testing.M) { os.Exit(audio.RunWithoutRealDevices(storesClosedAfter{m})) }

// storesClosedAfter runs m's tests, then closes freshStores.
type storesClosedAfter struct{ m *testing.M }

func (s storesClosedAfter) Run() int {
	defer freshStores.close()
	return s.m.Run()
}
