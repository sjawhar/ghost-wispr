package audio

import (
	"os"
	"testing"
)

// TestMain fails this package's test binary if anything in it tried to open a
// real audio device, which the binary refuses (RunWithoutRealDevices); the
// other packages whose tests link this one use audiotest.Main, which this
// package cannot import.
func TestMain(m *testing.M) {
	os.Exit(RunWithoutRealDevices(m))
}
