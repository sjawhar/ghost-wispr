package audio

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
)

// ErrRealDeviceRefused is the error of an open of a real audio device in a
// test binary, which refuses them (RunWithoutRealDevices).
var ErrRealDeviceRefused = errors.New("audio: a test may not open a real audio device")

// realDevices guards the process's real audio devices. Every open of a
// capture or playback stream, and every PortAudio initialization (which
// connects to the sound server ALSA's default PCM names and lists its
// devices), asks it first (realDevice). The app never sets it. A test binary
// does, from TestMain (RunWithoutRealDevices), so that no test opens a
// microphone or a speaker: on a developer's machine the default devices can
// be a person's own microphone and speaker, or a sound server forwarded from
// another machine.
var realDevices struct {
	mu      sync.Mutex
	refuse  bool
	allowed int      // AllowRealDevices calls not yet undone
	reached []string // each refused open, with the stack that tried it
}

// realDevice is called before each real open of what. In a test binary that
// refuses them, it records the open, with the stack that tried it, and
// returns ErrRealDeviceRefused.
func realDevice(what string) error {
	realDevices.mu.Lock()
	defer realDevices.mu.Unlock()
	if !realDevices.refuse || realDevices.allowed > 0 {
		return nil
	}
	stack := make([]byte, 16<<10)
	stack = stack[:runtime.Stack(stack, false)]
	reached := fmt.Sprintf("%s, from:\n%s", what, stack)
	realDevices.reached = append(realDevices.reached, reached)
	_, _ = fmt.Fprintf(os.Stderr, "audio: a test reached a real audio device: %s\n", reached)
	return fmt.Errorf("%w: %s", ErrRealDeviceRefused, what)
}

// RunWithoutRealDevices runs a test binary's tests (m.Run) with every open of
// a real audio device refused, and returns m.Run's exit code, or 1 if any test
// tried one: each refused open is printed with the stack that tried it as it
// happens, and counted at the end. Each package whose tests link this package
// calls it from TestMain (audiotest.Main). The app never calls it.
func RunWithoutRealDevices(m interface{ Run() int }) int {
	realDevices.mu.Lock()
	realDevices.refuse = true
	realDevices.mu.Unlock()
	code := m.Run()
	if n := len(RealDevicesReached()); n > 0 {
		_, _ = fmt.Fprintf(os.Stderr, "FAIL: the tests tried to open a real audio device %d times (each printed above, with its stack)\n", n)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// AllowRealDevices lets real audio devices open until the function it returns
// runs: for a test that needs one (audiotest.UseRealDevices, which runs only
// in a build with the hardware tag), and for an open that reaches no device,
// such as of a PCM ALSA does not define.
func AllowRealDevices() (restore func()) {
	realDevices.mu.Lock()
	realDevices.allowed++
	realDevices.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			realDevices.mu.Lock()
			realDevices.allowed--
			realDevices.mu.Unlock()
		})
	}
}

// RealDevicesReached returns each open of a real audio device refused so far:
// what it was, and the stack that tried it.
func RealDevicesReached() []string {
	realDevices.mu.Lock()
	defer realDevices.mu.Unlock()
	return append([]string(nil), realDevices.reached...)
}
