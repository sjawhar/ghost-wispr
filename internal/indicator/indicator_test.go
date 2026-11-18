package indicator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLight struct {
	mu          sync.Mutex
	colour      [3]uint8
	calls       []string
	setErr      error
	readErr     error
	stuckOff    bool // takes writes but always reads back off
	serverDowns []time.Duration
}

func (l *fakeLight) SetRGB(r, g, b uint8) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "set")
	if l.setErr != nil {
		return l.setErr
	}
	l.colour = [3]uint8{r, g, b}
	return nil
}

func (l *fakeLight) ReadRGB() (uint8, uint8, uint8, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "read")
	if l.readErr != nil {
		return 0, 0, 0, l.readErr
	}
	if l.stuckOff {
		return 0, 0, 0, nil
	}
	return l.colour[0], l.colour[1], l.colour[2], nil
}

func (l *fakeLight) ServerDown(timeout time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "serverdown")
	l.serverDowns = append(l.serverDowns, timeout)
	return nil
}

func (l *fakeLight) BlackPattern() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "black-pattern")
	return nil
}

func (l *fakeLight) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "close")
	return nil
}

func (l *fakeLight) snapshot() ([]string, []time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...), append([]time.Duration(nil), l.serverDowns...)
}

func countCalls(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

func waitFor(t *testing.T, description string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestBlink1ReportBytes(t *testing.T) {
	if got, want := Blink1Report('n', [6]byte{255, 0, 0}), [8]byte{0x01, 'n', 255, 0, 0, 0, 0, 0}; got != want {
		t.Fatalf("red report = %v, want %v", got, want)
	}
	if got, want := Blink1Report('n', [6]byte{}), [8]byte{0x01, 'n', 0, 0, 0, 0, 0, 0}; got != want {
		t.Fatalf("off report = %v, want %v", got, want)
	}
	if got, want := Blink1Report('D', [6]byte{1, 0x01, 0x2c, 1, 0, 31}), [8]byte{0x01, 'D', 1, 0x01, 0x2c, 1, 0, 31}; got != want {
		t.Fatalf("serverdown report = %v, want %v", got, want)
	}
	if got, want := Blink1Report('P', [6]byte{0, 0, 0, 0, 0, 31}), [8]byte{0x01, 'P', 0, 0, 0, 0, 0, 31}; got != want {
		t.Fatalf("black pattern report = %v, want %v", got, want)
	}
	if got, want := Blink1Report('W', [6]byte{0xbe, 0xef, 0xca, 0xfe}), [8]byte{0x01, 'W', 0xbe, 0xef, 0xca, 0xfe, 0, 0}; got != want {
		t.Fatalf("save report = %v, want %v", got, want)
	}
}

func TestOpenBlink1WithNoDeviceSaysSo(t *testing.T) {
	sysfsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysfsRoot, "class", "hidraw"), 0o755); err != nil {
		t.Fatalf("create empty sysfs: %v", err)
	}
	if _, err := OpenBlink1(sysfsRoot, t.TempDir()); !errors.Is(err, ErrNoBlink1) {
		t.Fatalf("open empty sysfs error = %v, want ErrNoBlink1", err)
	}
}

func TestControllerWritesTheBlackPatternOnStart(t *testing.T) {
	light := &fakeLight{}
	controller := NewController(light, func() bool { return false }, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { controller.Run(ctx); close(done) }()
	waitFor(t, "black pattern and watchdog", func() bool {
		calls, _ := light.snapshot()
		return len(calls) >= 2
	})
	cancel()
	<-done
	calls, _ := light.snapshot()
	if calls[0] != "black-pattern" || calls[1] != "serverdown" {
		t.Fatalf("startup calls = %v, want black-pattern then serverdown", calls)
	}
}

func TestControllerRedOnlyWhileRecording(t *testing.T) {
	light := &fakeLight{}
	var recording atomic.Bool
	controller := NewController(light, recording.Load, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { controller.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitFor(t, "controller startup", func() bool {
		calls, _ := light.snapshot()
		return len(calls) >= 2
	})
	recording.Store(true)
	controller.Wake()
	waitFor(t, "recording red", func() bool {
		light.mu.Lock()
		defer light.mu.Unlock()
		return light.colour == [3]uint8{255, 0, 0}
	})
	calls, _ := light.snapshot()
	setCalls := countCalls(calls, "set")
	readCalls := countCalls(calls, "read")
	for range 2 {
		controller.Wake()
		readCalls++
		waitFor(t, "same-state controller update", func() bool {
			calls, _ := light.snapshot()
			return countCalls(calls, "read") >= readCalls
		})
	}
	calls, _ = light.snapshot()
	if gotSetCalls := countCalls(calls, "set"); gotSetCalls != setCalls {
		t.Fatalf("colour writes while already red = %d, want %d", gotSetCalls, setCalls)
	}
	recording.Store(false)
	controller.Wake()
	waitFor(t, "recording off", func() bool {
		light.mu.Lock()
		defer light.mu.Unlock()
		return light.colour == [3]uint8{}
	})
}

func TestControllerRewritesAWrongColour(t *testing.T) {
	light := &fakeLight{}
	controller := NewController(light, func() bool { return true }, time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { controller.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitFor(t, "initial red", func() bool {
		light.mu.Lock()
		defer light.mu.Unlock()
		return light.colour == [3]uint8{255, 0, 0}
	})
	light.mu.Lock()
	light.colour = [3]uint8{}
	light.mu.Unlock()
	waitFor(t, "red restored by tick", func() bool {
		light.mu.Lock()
		defer light.mu.Unlock()
		return light.colour == [3]uint8{255, 0, 0}
	})
}

func TestControllerTicklesBeforeTheTimeout(t *testing.T) {
	light := &fakeLight{}
	controller := NewController(light, func() bool { return false }, time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { controller.Run(ctx); close(done) }()
	waitFor(t, "repeated watchdog tickle", func() bool {
		_, ticks := light.snapshot()
		return len(ticks) >= 3
	})
	cancel()
	<-done
	_, ticks := light.snapshot()
	if ticks[0] != 3*time.Second || ticks[1] != 3*time.Second {
		t.Fatalf("watchdog ticks = %v, want repeated 3s timeout", ticks)
	}
}

func TestControllerTurnsOffOnStop(t *testing.T) {
	light := &fakeLight{}
	controller := NewController(light, func() bool { return true }, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { controller.Run(ctx); close(done) }()
	waitFor(t, "initial red", func() bool {
		light.mu.Lock()
		defer light.mu.Unlock()
		return light.colour == [3]uint8{255, 0, 0}
	})
	cancel()
	<-done
	calls, ticks := light.snapshot()
	if len(calls) < 3 || calls[len(calls)-3] != "set" || calls[len(calls)-2] != "serverdown" || calls[len(calls)-1] != "close" {
		t.Fatalf("shutdown calls = %v, want final set, serverdown, close", calls)
	}
	if got := ticks[len(ticks)-1]; got != 0 {
		t.Fatalf("final watchdog timeout = %v, want disabled", got)
	}
	light.mu.Lock()
	finalColour := light.colour
	light.mu.Unlock()
	if finalColour != [3]uint8{} {
		t.Fatalf("final light colour = %v, want off", finalColour)
	}
}

// TestControllerKeepsTickingAfterLightErrors fails if a light error ends the
// controller's loop, which would let the device's watchdog decide alone.
func TestControllerKeepsTickingAfterLightErrors(t *testing.T) {
	light := &fakeLight{setErr: errors.New("disconnected")}
	controller := NewController(light, func() bool { return true }, time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { controller.Run(ctx); close(done) }()
	waitFor(t, "continued watchdog ticks", func() bool {
		_, ticks := light.snapshot()
		return len(ticks) >= 3
	})
	cancel()
	<-done
}

// TestControllerReportsWhetherTheLightShowsRed fails if a check while
// recording that cannot read, write or read back red goes unreported, since
// the recorder must then stop capture; or if a light that shows what the
// recording signal wants is reported as failed.
func TestControllerReportsWhetherTheLightShowsRed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		light     *fakeLight
		recording bool
		wantErr   error
	}{
		{name: "missing", light: &fakeLight{readErr: ErrNoBlink1}, recording: true, wantErr: ErrNoBlink1},
		{name: "missing while not recording", light: &fakeLight{readErr: ErrNoBlink1}, wantErr: ErrNoBlink1},
		{name: "write fails", light: &fakeLight{setErr: errDisconnected}, recording: true, wantErr: errDisconnected},
		{name: "reads back off after a red write", light: &fakeLight{stuckOff: true}, recording: true, wantErr: ErrNotRed},
		{name: "red while recording", light: &fakeLight{}, recording: true},
		{name: "off while not recording", light: &fakeLight{colour: [3]uint8{255, 0, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reports := make(chan error, 16)
			controller := NewController(tc.light, func() bool { return tc.recording }, time.Hour, func(err error) { reports <- err })
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { controller.Run(ctx); close(done) }()
			defer func() {
				cancel()
				<-done
			}()
			select {
			case err := <-reports:
				if tc.wantErr == nil && err != nil {
					t.Fatalf("check reported %v, want success", err)
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("check reported %v, want %v", err, tc.wantErr)
				}
			case <-time.After(time.Second):
				t.Fatal("the controller's first check reported nothing")
			}
		})
	}
}

var errDisconnected = errors.New("disconnected")
