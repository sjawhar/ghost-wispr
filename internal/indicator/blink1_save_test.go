//go:build linux && (amd64 || arm64)

package indicator

import (
	"os"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// flashBlink1 is a blink(1) whose pattern lines are in RAM, which 'P' writes
// and 'R' reads, and in flash, which 'W' saves RAM to and a power-up loads RAM
// from. Every line starts lit, as a factory pattern is.
type flashBlink1 struct {
	mu     sync.Mutex
	buffer [8]byte
	ram    [32][3]byte
	flash  [32][3]byte
	saves  int
}

func newFlashBlink1() *flashBlink1 {
	d := &flashBlink1{}
	for i := range d.ram {
		d.ram[i], d.flash[i] = [3]byte{255, 0, 0}, [3]byte{255, 0, 0}
	}
	return d
}

func (d *flashBlink1) ioctl(_ uintptr, request uintptr, report *[8]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if request == hidIOCGetFeature {
		*report = d.buffer
		return nil
	}
	switch report[1] {
	case 'v':
		d.buffer = [8]byte{0x01, 'v', 0, '3', '3'}
	case 'R':
		c := d.ram[report[7]%32]
		d.buffer = [8]byte{0x01, 'R', c[0], c[1], c[2], 0, 10, report[7]}
	case 'P':
		line := report[7] % 32
		d.ram[line] = [3]byte{report[2], report[3], report[4]}
		d.buffer = *report
	case 'W':
		d.flash = d.ram
		d.saves++
		d.buffer = *report
	default:
		d.buffer = *report
	}
	return nil
}

// TestASaveThatMissedTheUnitDoesNotMarkItKnownBlack fails if the black
// pattern check marks a unit known black by its serial number when its save
// ('W') never reached the blink(1), because another open held the node's lock
// past the save's wait. The read-back reads the lines in RAM, which 'P'
// wrote, so the check passes for this handle; but the saved pattern, which a
// power-up loads, stays lit. Plugged in again, a known unit would get no
// check and a tickle, and its watchdog would play that lit pattern (red team,
// review 5958478401 of #79). Control: with no other open at the save, the
// unit is saved black and known.
//
// The test runs the check one command at a time (PatternCheck), so another
// open holds the lock for exactly the save's step: it takes the free lock just
// before that step and lets it go just after. No goroutine races the check for
// the lock, and no sleep has to outlast one wait and fit inside another, so
// the test does not depend on how fast a loaded machine runs it.
func TestASaveThatMissedTheUnitDoesNotMarkItKnownBlack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		holdAtSave bool
	}{
		{"control: no other open at the save", false},
		{"another open holds the lock through the save", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serial := "save-test-free"
			if tc.holdAtSave {
				serial = "save-test-held"
			}
			forget := func() { checkedMu.Lock(); delete(checkedUnits, serial); checkedMu.Unlock() }
			forget()
			t.Cleanup(forget)
			sysfsRoot, devRoot, node := fakeBlink1Sysfs(t)
			setBlink1Serial(t, sysfsRoot, serial)
			device := newFlashBlink1()
			original := featureIOCTL
			t.Cleanup(func() { featureIOCTL = original })
			featureIOCTL = device.ioctl

			holder, err := os.Open(node)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = holder.Close() })
			light, err := OpenBlink1(sysfsRoot, devRoot)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = light.Close() })

			check, ok := NewPatternCheck(light).(*blink1PatternCheck)
			if !ok {
				t.Fatalf("the blink(1)'s pattern check is %T, want *blink1PatternCheck", NewPatternCheck(light))
			}
			step := func() (done bool) {
				t.Helper()
				done, err := check.Step()
				if err != nil {
					t.Fatalf("the check, whose read-back of RAM is black: %v, want it to pass for this handle", err)
				}
				return done
			}
			// The pattern is lit, so the check writes it black ('P' per line),
			// up to the save.
			for steps := 0; check.phase != checkSave; steps++ {
				if steps == 100 {
					t.Fatal("control: the check never reached its save")
				}
				if step() {
					t.Fatal("control: the check ended before it wrote the black pattern")
				}
			}
			if tc.holdAtSave {
				// The lock is free between two commands: another open takes it.
				if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatalf("control: another open could not take the free lock: %v", err)
				}
			}
			step() // the save ('W'); in the held case its wait for the lock runs out
			if tc.holdAtSave {
				if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
					t.Fatal(err)
				}
			}
			for steps := 0; ; steps++ {
				if steps == 100 {
					t.Fatal("control: the check never ended")
				}
				if step() {
					break
				}
			}

			device.mu.Lock()
			saves, flashBlack := device.saves, device.flash == [32][3]byte{}
			device.mu.Unlock()
			if !tc.holdAtSave {
				if saves != 1 || !flashBlack || !KnownBlack(light) {
					t.Fatalf("control: the check with no other open saved %d times, flash black %t, known %t; want it saved once, black, and known", saves, flashBlack, KnownBlack(light))
				}
				return
			}
			if saves != 0 {
				t.Fatalf("control: the save reached the blink(1) (%d saves) while another open held the lock", saves)
			}
			device.mu.Lock()
			device.ram = device.flash // plugged in again: RAM is loaded from flash
			device.mu.Unlock()
			reopened, err := OpenBlink1(sysfsRoot, devRoot)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if KnownBlack(reopened) {
				t.Fatalf("false black: the unit %q is known black though its save ('W') never reached it, so its saved pattern is still lit", serial)
			}
		})
	}
}
