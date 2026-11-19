//go:build linux && (amd64 || arm64)

package indicator

import (
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// flashBlink1 is a blink(1) whose pattern lines are in RAM, which 'P' writes
// and 'R' reads, and in flash, which 'W' saves RAM to and a power-up loads RAM
// from. Every line starts lit, as a factory pattern is. atLastP runs once
// inside the SET of the 'P' for line 31.
type flashBlink1 struct {
	mu      sync.Mutex
	buffer  [8]byte
	ram     [32][3]byte
	flash   [32][3]byte
	saves   int
	atLastP func()
	ran     bool
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
	if request == hidIOCGetFeature {
		*report = d.buffer
		d.mu.Unlock()
		return nil
	}
	var hook func()
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
		if line == 31 && d.atLastP != nil && !d.ran {
			d.ran, hook = true, d.atLastP
		}
	case 'W':
		d.flash = d.ram
		d.saves++
		d.buffer = *report
	default:
		d.buffer = *report
	}
	d.mu.Unlock()
	if hook != nil {
		hook()
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
			original, originalWait := featureIOCTL, exchangeWait
			t.Cleanup(func() { featureIOCTL, exchangeWait = original, originalWait })
			featureIOCTL = device.ioctl
			exchangeWait = 200 * time.Millisecond

			holder, err := os.Open(node)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = holder.Close() })
			released := make(chan error, 1)
			if tc.holdAtSave {
				device.atLastP = func() {
					go func() {
						// Takes the lock as soon as the last 'P' lets go, and holds
						// it 300 ms: past the save's 200 ms wait, inside the next read's.
						if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
							released <- err
							return
						}
						time.Sleep(300 * time.Millisecond)
						released <- unix.Flock(int(holder.Fd()), unix.LOCK_UN)
					}()
					time.Sleep(20 * time.Millisecond) // the other open waits for the lock before this 'P' lets go
				}
			}

			light, err := OpenBlink1(sysfsRoot, devRoot)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = light.Close() })
			checkErr := light.BlackPattern()
			if tc.holdAtSave {
				select {
				case err := <-released:
					if err != nil {
						t.Fatalf("control: the other open's lock: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("control: the other open never let go")
				}
			}
			device.mu.Lock()
			saves, flashBlack := device.saves, device.flash == [32][3]byte{}
			device.mu.Unlock()
			if !tc.holdAtSave {
				if checkErr != nil || saves != 1 || !flashBlack || !KnownBlack(light) {
					t.Fatalf("control: the check with no other open = %v, saves %d, flash black %t, known %t; want it passed, saved and known", checkErr, saves, flashBlack, KnownBlack(light))
				}
				return
			}
			if saves != 0 {
				t.Fatalf("control: the save reached the blink(1) (%d saves): the other open did not take the lock before it", saves)
			}
			if checkErr != nil {
				t.Fatalf("the check, whose read-back of RAM is black: %v, want it to pass for this handle", checkErr)
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
