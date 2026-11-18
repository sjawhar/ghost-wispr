//go:build linux && (amd64 || arm64)

package indicator

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeBlink1Sysfs makes a sysfs root and a dev root that list one blink(1),
// hidraw0, and returns them with its node.
func fakeBlink1Sysfs(t *testing.T) (sysfsRoot, devRoot, node string) {
	t.Helper()
	sysfsRoot, devRoot = t.TempDir(), t.TempDir()
	device := filepath.Join(sysfsRoot, "class", "hidraw", "hidraw0", "device")
	if err := os.MkdirAll(device, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(device, "uevent"), []byte("HID_ID=0003:000027B8:000001ED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	node = filepath.Join(devRoot, "hidraw0")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return sysfsRoot, devRoot, node
}

// TestLightOffReachesALightAnotherOpenHolds fails if `ghost-wispr light-off`
// sends nothing while another open of the node holds the lock. Its open's
// version query could not get the light, so the open failed before the off
// write, a lone SET that goes without the lock once its wait runs out (review
// of #79). The version waits until a command needs it.
func TestLightOffReachesALightAnotherOpenHolds(t *testing.T) {
	sysfsRoot, devRoot, node := fakeBlink1Sysfs(t)
	var (
		mu   sync.Mutex
		sets [][8]byte
	)
	original, originalWait := featureIOCTL, commandWait
	t.Cleanup(func() { featureIOCTL, commandWait = original, originalWait })
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		mu.Lock()
		defer mu.Unlock()
		if request == hidIOCSetFeature {
			sets = append(sets, *report)
		} else if report[1] == 'v' {
			report[3] = '3'
		}
		return nil
	}
	commandWait = 100 * time.Millisecond

	holder, err := os.Open(node)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	light, err := OpenBlink1ForCommand(sysfsRoot, devRoot)
	if err != nil {
		t.Fatalf("light-off's open of a blink(1) another open holds: %v, want the light", err)
	}
	t.Cleanup(func() { _ = light.Close() })
	if err := light.SetRGB(0, 0, 0); err != nil {
		t.Fatalf("light-off's off write to a blink(1) another open holds: %v", err)
	}
	mu.Lock()
	got := append([][8]byte(nil), sets...)
	mu.Unlock()
	if want := Blink1Report('n', [6]byte{}); len(got) != 1 || got[0] != want {
		t.Fatalf("reports sent while another open held the lock = %v, want one off write %v", got, want)
	}

	// Control: once the other open lets go, a command that needs the version
	// reads it.
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := light.ServerDown(3 * time.Second); err != nil {
		t.Fatalf("watchdog tickle once the light is free: %v", err)
	}
	if lines := light.(*blink1).patternLines; lines != 32 {
		t.Fatalf("pattern lines after the version was read = %d, want 32", lines)
	}
}

// TestAWaitingRecorderGoesBeforeLightState fails if the recorder's reads do
// not get the light within their wait while light-state runs back to back in
// four other processes. Such a read is unknown, and the gate drops that buffer
// of audio. The lock serves no queue, and the recorder polled for it as often
// as light-state did, so it lost the light to them: with 4 light-state loops
// on a uhid blink(1), 18 buffers were dropped in 20 s (review of #79). A
// waiting recorder must go first; light-state waits longer, and can.
func TestAWaitingRecorderGoesBeforeLightState(t *testing.T) {
	sysfsRoot, devRoot, _ := fakeBlink1Sysfs(t)
	original := featureIOCTL
	t.Cleanup(func() { featureIOCTL = original })
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		time.Sleep(2 * time.Millisecond) // a USB control transfer: the sender holds the lock meanwhile
		if request == hidIOCGetFeature {
			switch report[1] {
			case 'v':
				report[3] = '3'
			case 'r':
				report[2] = 255
			}
		}
		return nil
	}
	recorder, err := OpenBlink1(sysfsRoot, devRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })

	stop := make(chan struct{})
	var loops sync.WaitGroup
	var runs atomic.Int32
	for range 4 {
		loops.Add(1)
		go func() {
			defer loops.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				light, err := OpenBlink1ForCommand(sysfsRoot, devRoot)
				if err != nil {
					continue
				}
				if _, _, _, err := light.ReadRGB(); err == nil {
					runs.Add(1)
				}
				_ = light.Close()
			}
		}()
	}

	reads, unknown := 0, 0
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		_, _, _, err := recorder.ReadRGB()
		reads++
		if Contended(err) {
			unknown++
		} else if err != nil {
			t.Fatalf("the recorder's read: %v", err)
		}
	}
	close(stop)
	loops.Wait()
	if n := runs.Load(); n < 20 {
		t.Fatalf("control: light-state read the light %d times in 1.5 s, want at least 20", n)
	}
	if unknown > 0 {
		t.Fatalf("the recorder's read got no light %d of %d times while light-state ran back to back in 4 processes, want 0", unknown, reads)
	}
	t.Logf("the recorder read the light %d times; light-state read it %d times", reads, runs.Load())
}
