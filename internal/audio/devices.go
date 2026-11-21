package audio

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
)

// ErrRealDeviceRefused is the error of an open of a real audio device in a
// test binary, which refuses them (realDevices).
var ErrRealDeviceRefused = errors.New("audio: a test may not open a real audio device")

// deviceGuard guards the process's real audio devices (realDevices).
type deviceGuard struct {
	mu      sync.Mutex
	refuse  bool
	allowed int      // AllowRealDevices calls not yet undone
	reached []string // each refused open, with the stack that tried it
}

// realDevices guards the process's real audio devices. Every open of a
// capture or playback stream, and every PortAudio initialization (which
// connects to the sound server ALSA's default PCM names and lists its
// devices), asks it first (realDevice). In a binary `go test` built
// (testing.Testing), it refuses them from the moment this package
// initializes: before any package that imports it initializes, so an open in
// a package variable's initializer or an init function is refused too, and
// before TestMain. On a developer's machine the default devices can be a
// person's own microphone and speaker, or a sound server forwarded from
// another machine. The app, which `go build` builds, refuses nothing.
var realDevices = &deviceGuard{refuse: testing.Testing()}

// realDevice is called before each real open of what. In a test binary, it
// records the open, with the stack that tried it, and returns
// ErrRealDeviceRefused.
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

// RunWithoutRealDevices runs a test binary's tests (m.Run) and returns m.Run's
// exit code, or 1 if anything in the binary tried to open a real audio
// device, in a test or as a package initialized: each refused open is printed
// with the stack that tried it as it happens, and counted at the end. Each
// package whose tests link this package calls it from TestMain
// (audiotest.Main), since a refused open's error alone can be ignored. The app
// never calls it.
func RunWithoutRealDevices(m interface{ Run() int }) int {
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
