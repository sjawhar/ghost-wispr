// Package audiotest keeps tests off real audio devices. In a binary `go test`
// built, internal/audio refuses every open of a real microphone or speaker
// stream and every PortAudio initialization, from the moment it initializes,
// before any package that imports it. Main, called from the TestMain of every
// package whose tests link internal/audio, fails the test binary if anything
// in it tried one (audio.RunWithoutRealDevices), since a refused open's error
// alone can be ignored. A test that must reach a real device calls
// UseRealDevices, which runs it only in a build with the hardware tag; CI never
// sets it.
package audiotest

import (
	"os"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Main runs m's tests and exits with their code, or 1 if anything in the test
// binary, a test or a package as it initialized, tried to open a real device.
func Main(m *testing.M) {
	os.Exit(audio.RunWithoutRealDevices(m))
}

// UseRealDevices lets t open real audio devices. It skips t unless the test
// binary was built with the hardware tag, which CI never sets: such a test
// needs a machine with the device, and a person who knows it records or plays.
func UseRealDevices(t testing.TB) {
	t.Helper()
	if !hardware {
		t.Skip("opens a real audio device: runs only with -tags hardware")
	}
	t.Cleanup(audio.AllowRealDevices())
}
