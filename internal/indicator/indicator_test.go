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

func TestOpenBlink1FindsTheDeviceBySysfs(t *testing.T) {
	sysfsRoot := t.TempDir()
	devRoot := t.TempDir()
	for _, name := range []string{"hidraw0", "hidraw3"} {
		if err := os.MkdirAll(filepath.Join(sysfsRoot, "class", "hidraw", name, "device"), 0o755); err != nil {
			t.Fatalf("create sysfs entry: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(sysfsRoot, "class", "hidraw", "hidraw0", "device", "uevent"), []byte("HID_ID=0003:00001234:00005678\n"), 0o644); err != nil {
		t.Fatalf("write unrelated uevent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysfsRoot, "class", "hidraw", "hidraw3", "device", "uevent"), []byte("HID_ID=0003:000027B8:000001ED\n"), 0o644); err != nil {
		t.Fatalf("write blink uevent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devRoot, "hidraw3"), nil, 0o600); err != nil {
		t.Fatalf("create fake hidraw device: %v", err)
	}
	original := featureIOCTL
	featureIOCTL = func(_ uintptr, _ uintptr, report *[8]byte) error {
		if report[1] == 'v' {
			report[3] = '3'
		}
		return nil
	}
	t.Cleanup(func() { featureIOCTL = original })

	light, err := OpenBlink1(sysfsRoot, devRoot)
	if err != nil {
		t.Fatalf("open blink(1): %v", err)
	}
	t.Cleanup(func() { _ = light.Close() })
	if got := light.(*blink1).file.Name(); got != filepath.Join(devRoot, "hidraw3") {
		t.Fatalf("opened device = %q, want %q", got, filepath.Join(devRoot, "hidraw3"))
	}
	if got := light.(*blink1).patternLines; got != 32 {
		t.Fatalf("mk3 pattern lines = %d, want 32", got)
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

func TestReadRGBSendsThenGets(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "hidraw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	light := &blink1{file: file, patternLines: 32}
	original := featureIOCTL
	var requests []uintptr
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		requests = append(requests, request)
		if request == hidIOCGetFeature {
			report[2], report[3], report[4] = 255, 0, 0
		}
		return nil
	}
	t.Cleanup(func() { featureIOCTL = original })

	r, g, b, err := light.ReadRGB()
	if err != nil {
		t.Fatalf("read rgb: %v", err)
	}
	if got, want := [3]uint8{r, g, b}, [3]uint8{255, 0, 0}; got != want {
		t.Fatalf("colour = %v, want %v", got, want)
	}
	if got, want := requests, []uintptr{hidIOCSetFeature, hidIOCGetFeature}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ioctl requests = %v, want %v", got, want)
	}
}

func TestBlink1BlackPatternAndWatchdogReports(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "hidraw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	light := &blink1{file: file, patternLines: 32}
	original := featureIOCTL
	var reports [][8]byte
	patterns := make(map[byte][8]byte)
	var patternReads []byte
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		if request == hidIOCSetFeature {
			reports = append(reports, *report)
			if report[1] == 'P' {
				patterns[report[7]] = *report
			}
			return nil
		}
		if report[1] == 'R' {
			line := report[7]
			patternReads = append(patternReads, line)
			pattern, ok := patterns[line]
			if !ok || pattern[2] != 0 || pattern[3] != 0 || pattern[4] != 0 || (pattern[5] == 0 && pattern[6] == 0) {
				report[2] = 1
			} else {
				report[6] = pattern[6]
			}
		}
		return nil
	}
	t.Cleanup(func() { featureIOCTL = original })

	if err := light.BlackPattern(); err != nil {
		t.Fatalf("black pattern: %v", err)
	}
	if err := light.ServerDown(3 * time.Second); err != nil {
		t.Fatalf("serverdown: %v", err)
	}
	if got, want := reports[1], Blink1Report('l', [6]byte{}); got != want {
		t.Fatalf("all-LED report = %v, want %v", got, want)
	}
	var patternsWritten [][8]byte
	lastPattern := -1
	saveIndex := -1
	for i, report := range reports {
		switch report[1] {
		case 'P':
			patternsWritten = append(patternsWritten, report)
			lastPattern = i
		case 'W':
			saveIndex = i
		}
	}
	if got, want := len(patternsWritten), 32; got != want {
		t.Fatalf("saved pattern lines = %d, want %d", got, want)
	}
	for line := range 32 {
		if got, want := patternsWritten[line], Blink1Report('P', [6]byte{0, 0, 0, 0, 10, byte(line)}); got != want {
			t.Fatalf("pattern line %d = %v, want %v", line, got, want)
		}
	}
	if saveIndex != lastPattern+1 || reports[saveIndex] != Blink1Report('W', [6]byte{0xbe, 0xef, 0xca, 0xfe}) {
		t.Fatalf("save report at %d after final pattern %d = %v, want immediate save", saveIndex, lastPattern, reports[saveIndex])
	}
	if got, want := len(patternReads), 33; got != want || patternReads[0] != 0 {
		t.Fatalf("pattern reads = %v, want initial line 0 then 32 verification reads", patternReads)
	}
	for line := range 32 {
		if got := patternReads[line+1]; got != byte(line) {
			t.Fatalf("verification read %d = line %d, want line %d", line, got, line)
		}
	}
	if got, want := reports[len(reports)-1], Blink1Report('D', [6]byte{1, 0x01, 0x2c, 1, 0, 31}); got != want {
		t.Fatalf("watchdog report = %v, want %v", got, want)
	}
}

func TestControllerWritesTheBlackPatternOnStart(t *testing.T) {
	light := &fakeLight{}
	controller := NewController(light, func() bool { return false }, time.Hour)
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
	controller := NewController(light, recording.Load, time.Hour)
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
	controller := NewController(light, func() bool { return true }, time.Millisecond)
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
	controller := NewController(light, func() bool { return false }, time.Millisecond)
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
	controller := NewController(light, func() bool { return true }, time.Hour)
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

func TestControllerLightErrorsDoNotStopRecording(t *testing.T) {
	light := &fakeLight{setErr: errors.New("disconnected")}
	controller := NewController(light, func() bool { return true }, time.Millisecond)
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
