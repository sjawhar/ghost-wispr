//go:build !hardware

package audiotest_test

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio/audiotest"
)

// TestUseRealDevicesSkipsWithoutTheHardwareTag fails if a test that asks for
// real audio devices runs in a build without the hardware tag, as CI's.
func TestUseRealDevicesSkipsWithoutTheHardwareTag(t *testing.T) {
	var skipped, ran bool
	t.Run("a test that needs a real device", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		audiotest.UseRealDevices(t)
		ran = true
	})
	if !skipped || ran {
		t.Fatalf("UseRealDevices without the hardware tag: skipped %t, ran on %t; want it skipped", skipped, ran)
	}
}
