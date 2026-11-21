package audio

import (
	"os"
	"testing"
)

// TestMain runs this package's tests with every real audio device refused
// (RunWithoutRealDevices); the other packages whose tests link this one use
// audiotest.Main, which this package cannot import.
func TestMain(m *testing.M) {
	os.Exit(RunWithoutRealDevices(m))
}
