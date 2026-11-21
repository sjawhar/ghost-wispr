package audio

import (
	"errors"
	"strings"
	"testing"

	"github.com/gordonklaus/portaudio"
)

// forgetRefusedOpens drops, when t ends, the refused opens recorded during it:
// the opens a test of the guard makes on purpose, which would otherwise fail
// the test binary (RunWithoutRealDevices).
func forgetRefusedOpens(t *testing.T) {
	t.Helper()
	before := len(RealDevicesReached())
	t.Cleanup(func() {
		realDevices.mu.Lock()
		realDevices.reached = realDevices.reached[:before]
		realDevices.mu.Unlock()
	})
}

// refusedOpen is one open of a real audio device the guard must refuse.
type refusedOpen struct {
	what string // how the guard names it
	open func() error
}

// checkRefused fails t unless each open is refused with ErrRealDeviceRefused
// and recorded, named, with the stack that tried it.
func checkRefused(t *testing.T, opens []refusedOpen) {
	t.Helper()
	forgetRefusedOpens(t)
	for _, o := range opens {
		before := len(RealDevicesReached())
		if err := o.open(); !errors.Is(err, ErrRealDeviceRefused) {
			t.Errorf("%s in a test binary = %v, want ErrRealDeviceRefused", o.what, err)
			continue
		}
		reached := RealDevicesReached()
		if len(reached) != before+1 || !strings.HasPrefix(reached[before], o.what) || !strings.Contains(reached[before], "devices_") {
			t.Errorf("%s: recorded %q, want it named with the stack that tried it", o.what, reached[before:])
		}
	}
}

// TestATestBinaryRefusesEveryRealPortAudioDevice fails if, in a test binary,
// whose TestMain refuses real audio devices, PortAudio's default input, a
// named input or PortAudio's initialization is not refused and recorded. The
// devices are ones no machine has, and PortAudio is not initialized, so a
// broken guard still opens nothing. The initialization comes last: a broken
// guard initializes PortAudio, which the test then terminates.
func TestATestBinaryRefusesEveryRealPortAudioDevice(t *testing.T) {
	checkRefused(t, []refusedOpen{
		{"PortAudio's default input", func() error {
			_, err := paOpenDefaultInput(16000, 4000, new([]int16))
			return err
		}},
		{"PortAudio input gw-no-such-input", func() error {
			params := portaudio.StreamParameters{Input: portaudio.StreamDeviceParameters{Device: &portaudio.DeviceInfo{Name: "gw-no-such-input"}, Channels: 1}}
			_, err := paOpenInput(params, new([]int16))
			return err
		}},
		{"PortAudio's initialization", func() error {
			err := paInitialize()
			if err == nil {
				_ = portaudio.Terminate()
			}
			return err
		}},
	})
}

// TestAllowRealDevicesLetsAnOpenThrough fails if an open is still refused
// while AllowRealDevices holds, or is let through once it is undone: the
// default input then reaches PortAudio, which is not initialized here, so it
// opens nothing and answers PortAudio's own error.
func TestAllowRealDevicesLetsAnOpenThrough(t *testing.T) {
	forgetRefusedOpens(t)
	restore := AllowRealDevices()
	_, err := paOpenDefaultInput(16000, 4000, new([]int16))
	restore()
	if err == nil || errors.Is(err, ErrRealDeviceRefused) {
		t.Fatalf("the default input while allowed = %v, want PortAudio's not-initialized error", err)
	}
	restore() // a second undo does nothing
	if _, err := paOpenDefaultInput(16000, 4000, new([]int16)); !errors.Is(err, ErrRealDeviceRefused) {
		t.Fatalf("the default input once the allowance is undone = %v, want ErrRealDeviceRefused", err)
	}
}
