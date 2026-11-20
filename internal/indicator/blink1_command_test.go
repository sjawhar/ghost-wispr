//go:build linux && (amd64 || arm64)

package indicator

import (
	"fmt"
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

// TestACommandOpenDoesNotFailForAnotherCommand fails if a subcommand's open
// (`light-state`, `light-off`) fails because another command had the light at
// its version query: the lock was held past its wait, or another command's
// reply answered the query. On a uhid blink(1), light-state printed nothing
// for a version query that the recorder's tickle answered (review of #79).
// The open must return the light, and a read once the light is free reads it.
func TestACommandOpenDoesNotFailForAnotherCommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hold  bool // another open holds the lock at the open
		other bool // another command's reply answers the version query
	}{
		{name: "the lock held past the wait", hold: true},
		{name: "the version query answered by another command", other: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sysfsRoot, devRoot, node := fakeBlink1Sysfs(t)
			var answeredByOther atomic.Bool
			answeredByOther.Store(tc.other)
			original, originalWait := featureIOCTL, commandWait
			t.Cleanup(func() { featureIOCTL, commandWait = original, originalWait })
			commandWait = 50 * time.Millisecond
			featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
				if request != hidIOCGetFeature {
					return nil
				}
				switch report[1] {
				case 'v':
					if answeredByOther.Load() {
						*report = Blink1Report('D', [6]byte{1, 1, 44})
					} else {
						report[3] = '3'
					}
				case 'r':
					report[2] = 255
				}
				return nil
			}
			holder, err := os.Open(node)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = holder.Close() })
			if tc.hold {
				if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
					t.Fatal(err)
				}
			}

			light, err := OpenBlink1ForCommand(sysfsRoot, devRoot)
			if err != nil {
				t.Fatalf("a subcommand's open with %s: %v, want the light", tc.name, err)
			}
			t.Cleanup(func() { _ = light.Close() })

			answeredByOther.Store(false)
			if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			if r, g, b, err := light.ReadRGB(); err != nil || r != 255 || g != 0 || b != 0 {
				t.Fatalf("control: read once the light is free = %d,%d,%d, %v, want 255,0,0", r, g, b, err)
			}
		})
	}
}

// lockOrderSeeds seed the lock simulations of the lock order tests: each gives
// the loops other start instants, and the sleeps other lateness.
var lockOrderSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8}

// lockOrderLateness is how late a sleep can end in a lock simulation: Go's
// runtime waits for a timer in whole milliseconds (recorderPoll).
const lockOrderLateness = time.Millisecond

// lockOrderRun is what a recorder and light-state loops did on the lock
// simulation's clock.
type lockOrderRun struct {
	reads, unknown int // the recorder's reads that got the light, and those that did not
	held           int // the recorder's polls: times it found the node's lock held
	lightState     int // light-state's reads
}

// runLockOrder runs, on a lock simulation from seed, a recorder that reads its
// light reads times, 10 ms apart, while loops light-state loops, started at
// instants in the first 5 ms, open the light with open, read it and close it,
// back to back. Each transfer takes 2 ms, with the node's lock held.
func runLockOrder(t *testing.T, seed uint64, reads, loops int, open func(sysfsRoot, devRoot string) (Light, error)) lockOrderRun {
	t.Helper()
	sysfsRoot, devRoot, _ := fakeBlink1Sysfs(t)
	original := featureIOCTL
	t.Cleanup(func() { featureIOCTL = original })
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		lockClock.Sleep(2 * time.Millisecond) // a USB control transfer: the sender holds the lock meanwhile
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
	sim := newLockSim(t, seed, lockOrderLateness)
	var run lockOrderRun
	var stopped atomic.Bool
	var lightState atomic.Int32
	recorderActor := sim.Go("recorder", true, 0, func() {
		defer stopped.Store(true)
		// Opened here, its exchanges run on the simulation's clock only.
		recorder, err := OpenBlink1(sysfsRoot, devRoot)
		if err != nil {
			sim.fail("the recorder's open: %v", err)
			return
		}
		defer func() { _ = recorder.Close() }()
		for range reads {
			switch _, _, _, err := recorder.ReadRGB(); {
			case Contended(err):
				run.unknown++
			case err != nil:
				sim.fail("the recorder's read: %v", err)
				return
			default:
				run.reads++
			}
			lockClock.Sleep(10 * time.Millisecond)
		}
	})
	for i := range loops {
		sim.Go(fmt.Sprintf("light-state %d", i+1), false, sim.Offset(5*time.Millisecond), func() {
			for !stopped.Load() {
				light, err := open(sysfsRoot, devRoot)
				if err != nil {
					sim.fail("light-state's open: %v", err)
					return
				}
				if _, _, _, err := light.ReadRGB(); err == nil {
					lightState.Add(1)
				} else if !Contended(err) {
					sim.fail("light-state's read: %v", err)
				}
				_ = light.Close()
			}
		})
	}
	sim.Run(30 * time.Second)
	run.held, run.lightState = sim.Sleeps(recorderActor, recorderPoll), int(lightState.Load())
	return run
}

// TestAWaitingRecorderGoesBeforeLightState fails if the recorder's reads do
// not get the light within their wait while light-state runs back to back in
// four other handles. Such a read is unknown, and the gate drops that buffer
// of audio. The lock serves no queue, and the recorder polled for it as often
// as light-state did, so it lost the light to them: with 4 light-state loops
// on a uhid blink(1), 18 buffers were dropped in 20 s (review of #79). A
// waiting recorder must go first; light-state waits longer, and can.
//
// The recorder goes first because it tries the lock every recorderPoll and
// light-state every commandPoll, after a gap of exchangeGap. Run on the real
// clock, that order was only as good as the machine's timers: under load a
// sleep of 1 ms ended after 5 ms, and the test failed with the order intact
// (1 of 50 runs at load 680 to 2,240 on sami-devbox). So the handles run on a
// lock simulation (lockSim), whose sleeps end up to 1 ms late as an idle
// machine's do, and which wakes light-state first when two wake at the same
// instant. Control: with light-state handles that wait and poll as the
// recorder does (OpenBlink1), the recorder's reads lose the light.
func TestAWaitingRecorderGoesBeforeLightState(t *testing.T) {
	for _, seed := range lockOrderSeeds {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			run := runLockOrder(t, seed, 100, 4, OpenBlink1ForCommand)
			t.Logf("the recorder read the light %d times, found it held %d times; light-state read it %d times", run.reads, run.held, run.lightState)
			if run.unknown > 0 {
				t.Fatalf("the recorder's read got no light %d of %d times while light-state ran back to back in 4 handles, want 0", run.unknown, run.reads+run.unknown)
			}
			if run.held == 0 || run.lightState < 20 {
				t.Fatalf("control: the recorder found the light held %d times and light-state read it %d times, want at least once and 20 times: the loops did not contend", run.held, run.lightState)
			}
		})
		t.Run(fmt.Sprint("control: light-state polling as the recorder does, seed ", seed), func(t *testing.T) {
			run := runLockOrder(t, seed, 100, 4, OpenBlink1)
			t.Logf("the recorder read the light %d times, %d unknown, found it held %d times; light-state read it %d times", run.reads, run.unknown, run.held, run.lightState)
			if run.unknown == 0 {
				t.Fatal("with light-state handles that poll as the recorder does, every read of the recorder got the light: the test cannot see a lost lock order")
			}
		})
	}
}
