//go:build linux && (amd64 || arm64)

package indicator

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// flashBlink1 is a blink(1) whose pattern lines are in RAM, which 'P' writes
// and 'R' reads, and in flash, which 'W' saves RAM to and a power-up loads RAM
// from. Every line starts lit, as a factory pattern is. Each line also keeps
// the LED it plays on: the LED number 'l' last set when 'P' wrote it (0, every
// LED), which 'R' answers in byte 7, as both firmwares do.
type flashBlink1 struct {
	mu       sync.Mutex
	buffer   [8]byte
	ram      [32][3]byte
	flash    [32][3]byte
	ledn     byte
	ramLED   [32]byte
	flashLED [32]byte
	saves    int
	// saveErr, when set, fails each save's transfer: the save reaches no
	// flash.
	saveErr error
	// readErrs fail that many of the next line reads' transfers.
	readErrs int
	// dropWrites leaves RAM as it is at each 'P', as a unit that did not
	// take the write.
	dropWrites bool
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
		if d.buffer[1] == 'R' && d.readErrs > 0 {
			d.readErrs--
			return unix.EIO
		}
		*report = d.buffer
		return nil
	}
	switch report[1] {
	case 'v':
		d.buffer = [8]byte{0x01, 'v', 0, '3', '3'}
	case 'R':
		line := report[7] % 32
		c := d.ram[line]
		d.buffer = [8]byte{0x01, 'R', c[0], c[1], c[2], 0, 10, d.ramLED[line]}
	case 'l':
		d.ledn = report[2]
		d.buffer = *report
	case 'P':
		if !d.dropWrites {
			line := report[7] % 32
			d.ram[line] = [3]byte{report[2], report[3], report[4]}
			d.ramLED[line] = d.ledn
		}
		d.buffer = *report
	case 'W':
		if d.saveErr != nil {
			return d.saveErr
		}
		d.flash, d.flashLED = d.ram, d.ramLED
		d.saves++
		d.buffer = *report
	default:
		d.buffer = *report
	}
	return nil
}

// stepWait bounds a test's wait for one step of the check. A step whose wait
// for the node's lock had no bound would otherwise hold the test until go
// test's timeout while the test holds the lock.
const stepWait = 10 * time.Second

// saveRig is a blink(1) with flash, by serial number serial, behind a fake
// sysfs, and another open of its node, which can hold the node's lock between
// two of the check's commands, as `ghost-wispr light-state` does.
type saveRig struct {
	t                  *testing.T
	serial             string
	sysfsRoot, devRoot string
	node               string
	device             *flashBlink1
	holder             *os.File
}

func newSaveRig(t *testing.T, serial string) *saveRig {
	t.Helper()
	forget := func() { checkedMu.Lock(); delete(checkedUnits, serial); checkedMu.Unlock() }
	forget()
	t.Cleanup(forget)
	r := &saveRig{t: t, serial: serial, device: newFlashBlink1()}
	r.sysfsRoot, r.devRoot, r.node = fakeBlink1Sysfs(t)
	setBlink1Serial(t, r.sysfsRoot, serial)
	original := featureIOCTL
	t.Cleanup(func() { featureIOCTL = original })
	featureIOCTL = r.device.ioctl
	holder, err := os.Open(r.node)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	r.holder = holder
	return r
}

// open opens the unit for the recorder, as after a device error, closed at
// the test's end.
func (r *saveRig) open() Light {
	r.t.Helper()
	light, err := OpenBlink1(r.sysfsRoot, r.devRoot)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = light.Close() })
	return light
}

// check returns light's pattern check, as the recorder's CheckPattern builds
// one.
func (r *saveRig) check(light Light) *blink1PatternCheck {
	r.t.Helper()
	check, ok := NewPatternCheck(light).(*blink1PatternCheck)
	if !ok {
		r.t.Fatalf("the blink(1)'s pattern check is %T, want *blink1PatternCheck", NewPatternCheck(light))
	}
	return check
}

// hold takes the node's lock from the other open, between two of the
// check's commands, and makes sure a third open cannot take it.
func (r *saveRig) hold() {
	r.t.Helper()
	if err := unix.Flock(int(r.holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		r.t.Fatalf("another open could not take the node's lock between two of the check's commands (%v): the check must let it go after each command", err)
	}
	third, err := os.Open(r.node)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = third.Close() }()
	if err := unix.Flock(int(third.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		r.t.Fatalf("control: a third open's try for the lock the other open holds = %v, want EWOULDBLOCK: the test's hold holds nothing", err)
	}
}

// release lets the other open's lock go, if it holds it.
func (r *saveRig) release() {
	r.t.Helper()
	if err := unix.Flock(int(r.holder.Fd()), unix.LOCK_UN); err != nil {
		r.t.Fatal(err)
	}
}

// state is what the fake unit holds: how many saves reached it, whether its
// flash and its RAM are black.
func (r *saveRig) state() (saves int, flashBlack, ramBlack bool) {
	r.device.mu.Lock()
	defer r.device.mu.Unlock()
	return r.device.saves, r.device.flash == [32][3]byte{}, r.device.ram == [32][3]byte{}
}

// phaseName names a phase of the check for a failure message.
func phaseName(phase int) string {
	switch phase {
	case checkRead:
		return "read"
	case checkSelectLEDs:
		return "select LEDs ('l')"
	case checkWrite:
		return "write ('P')"
	case checkSave:
		return "save ('W')"
	}
	return "unknown"
}

// try runs one step of check, bounded by stepWait. A step still waiting then
// fails the test once the other open has let the lock go.
func (r *saveRig) try(check *blink1PatternCheck) (done bool, err error) {
	r.t.Helper()
	type result struct {
		done bool
		err  error
	}
	phase := check.phase
	stepped := make(chan result, 1)
	go func() {
		done, err := check.Step()
		stepped <- result{done, err}
	}()
	select {
	case got := <-stepped:
		return got.done, got.err
	case <-time.After(stepWait):
		r.release()
		<-stepped
		r.t.Fatalf("a step of the check in phase %s still waited for the node's lock after %v: its wait for a lock another open holds must be bounded (exchangeWait %v)", phaseName(phase), stepWait, exchangeWait)
		return false, nil
	}
}

// step runs one step of check, which must pass.
func (r *saveRig) step(check *blink1PatternCheck) (done bool) {
	r.t.Helper()
	phase, line := check.phase, check.line
	done, err := r.try(check)
	if err != nil {
		r.t.Fatalf("the check's step in phase %s, line %d: %v; want it to pass", phaseName(phase), line, err)
	}
	return done
}

// stepToSave steps check up to its save of the written black pattern, which
// must be a step of its own, right after the last 'P'.
func (r *saveRig) stepToSave(check *blink1PatternCheck) {
	r.t.Helper()
	for steps := 0; check.phase != checkSave; steps++ {
		if steps == 100 {
			r.t.Fatal("control: the check never reached its save")
		}
		if r.step(check) {
			r.t.Fatal("the check ended without a step of its own for the save ('W') of the black pattern it wrote: want the save sent alone, after the last 'P'")
		}
	}
}

// finish steps check to its end, each step passing.
func (r *saveRig) finish(check *blink1PatternCheck) {
	r.t.Helper()
	for steps := 0; !r.step(check); steps++ {
		if steps == 200 {
			r.t.Fatal("control: the check never ended")
		}
	}
}

// saveHeld runs the check's save step while the other open holds the lock,
// and requires that the save did not reach the unit.
func (r *saveRig) saveHeld(check *blink1PatternCheck) {
	r.t.Helper()
	r.hold()
	r.step(check) // the save ('W'); its wait for the lock runs out
	if saves, _, _ := r.state(); saves != 0 {
		r.t.Fatalf("the save reached the blink(1) (%d saves) though another open held the node's lock through its step: a save must wait for the lock, and send nothing once its wait runs out", saves)
	}
}

// requireNotFalseBlack fails the test if light's unit is known black while
// its flash, which a power-up loads, is not black.
func (r *saveRig) requireNotFalseBlack(light Light, what string) {
	r.t.Helper()
	saves, flashBlack, _ := r.state()
	if KnownBlack(light) && !flashBlack {
		r.t.Fatalf("false black (%s): the unit %q is known black, but no save reached it (%d saves) and its flash is lit", what, r.serial, saves)
	}
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
			r := newSaveRig(t, serial)
			light := r.open()
			check := r.check(light)
			// The pattern is lit, so the check writes it black ('P' per line),
			// up to the save.
			r.stepToSave(check)
			if tc.holdAtSave {
				r.saveHeld(check)
				r.release()
			} else {
				r.step(check)
			}
			r.finish(check)

			saves, flashBlack, _ := r.state()
			if !tc.holdAtSave {
				if saves != 1 || !flashBlack || !KnownBlack(light) {
					t.Fatalf("control: the check with no other open saved %d times, flash black %t, known %t; want it saved once, black, and known", saves, flashBlack, KnownBlack(light))
				}
				return
			}
			r.requireNotFalseBlack(light, "the check whose save missed")
			r.device.mu.Lock()
			r.device.ram = r.device.flash // plugged in again: RAM is loaded from flash
			r.device.mu.Unlock()
			r.requireNotFalseBlack(r.open(), "the unit plugged in again")
		})
	}
}

// TestALaterCheckAfterAMissedSaveSavesBeforeTheUnitIsKnown fails if a check
// that starts after a save that missed the unit marks the unit known black
// without a save of its own that reaches it. The missed save left RAM black,
// which the reads see, and flash lit, which a power-up loads. Before, only
// the check whose save missed knew of it, so a later check found RAM black,
// saved nothing and marked the unit known; after the next power cycle the
// recorder skipped the check of a known unit, and the watchdog played the lit
// pattern (review 5983673477 of #99). Each case runs the later check with the
// lock free, and requires it to save the unit black and know it.
func TestALaterCheckAfterAMissedSaveSavesBeforeTheUnitIsKnown(t *testing.T) {
	for _, tc := range []struct {
		name string
		// later runs the missed save, and returns the light and check that
		// come after it.
		later func(r *saveRig) (Light, *blink1PatternCheck)
	}{
		{
			// The recorder's checkStep returns the read-back's ErrBusy and
			// keeps the pattern unchecked; its next CheckPattern builds a new
			// check on the same handle.
			"a new check on the same handle, as the recorder's next CheckPattern builds",
			func(r *saveRig) (Light, *blink1PatternCheck) {
				light := r.open()
				check := r.check(light)
				r.stepToSave(check)
				r.saveHeld(check)
				if _, err := r.try(check); !Contended(err) { // the first read-back
					r.t.Fatalf("control: the first read-back with the lock still held = %v, want ErrBusy", err)
				}
				r.release()
				return light, r.check(light)
			},
		},
		{
			// A device error closed the handle; the unit was not power
			// cycled, so RAM is as the 'P's left it.
			"a new check on a handle opened again without a power cycle",
			func(r *saveRig) (Light, *blink1PatternCheck) {
				light := r.open()
				check := r.check(light)
				r.stepToSave(check)
				r.saveHeld(check)
				r.release()
				r.finish(check)
				r.requireNotFalseBlack(light, "the check whose save missed")
				_ = light.Close()
				reopened := r.open()
				return reopened, r.check(reopened)
			},
		},
		{
			// The recorder restarted, without a power cycle of the unit: the
			// new process knows nothing of the save that missed. Forgetting
			// the unit stands for the new process.
			"a check in a new process, after a save that missed in an earlier one",
			func(r *saveRig) (Light, *blink1PatternCheck) {
				light := r.open()
				check := r.check(light)
				r.stepToSave(check)
				r.saveHeld(check)
				r.release()
				r.finish(check)
				_ = light.Close()
				checkedMu.Lock()
				delete(checkedUnits, r.serial)
				checkedMu.Unlock()
				reopened := r.open()
				return reopened, r.check(reopened)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSaveRig(t, "later-check")
			light, later := tc.later(r)
			if saves, flashBlack, ramBlack := r.state(); saves != 0 || flashBlack || !ramBlack {
				t.Fatalf("control: before the later check, the unit has %d saves, flash black %t, RAM black %t; want 0, false, true", saves, flashBlack, ramBlack)
			}
			r.finish(later)
			r.requireNotFalseBlack(light, tc.name)
			if saves, flashBlack, _ := r.state(); saves != 1 || !flashBlack || !KnownBlack(light) {
				t.Fatalf("the later check left the unit with %d saves, flash black %t, known %t; want it saved once, black, and known", saves, flashBlack, KnownBlack(light))
			}
		})
	}
}

// TestASaveThatFailsOrAReadErrorNeverMarksTheUnitKnown fails if a save whose
// transfer failed, or a read error after a save that missed, leaves the unit
// known black while its flash is lit. A failed transfer is not ErrBusy, but
// it reaches no flash either; and the check the read error interrupts goes on
// with the next step, as the recorder's checkStep does, so the error must not
// make it forget its missed save (review 5983673477 of #99, the save test's
// gaps 3 and 4). A check that saved again after the error, and reached the
// unit, would pass: only a known unit with lit flash fails.
func TestASaveThatFailsOrAReadErrorNeverMarksTheUnitKnown(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(r *saveRig, check *blink1PatternCheck)
	}{
		{
			"the save's transfer fails",
			func(r *saveRig, check *blink1PatternCheck) {
				r.device.mu.Lock()
				r.device.saveErr = unix.EIO
				r.device.mu.Unlock()
				r.stepToSave(check)
				r.step(check) // the save, whose transfer fails
				r.finish(check)
			},
		},
		{
			"the first read-back after a missed save gets ErrBusy, and the check goes on",
			func(r *saveRig, check *blink1PatternCheck) {
				r.stepToSave(check)
				r.saveHeld(check)
				if _, err := r.try(check); !Contended(err) {
					r.t.Fatalf("control: the first read-back with the lock still held = %v, want ErrBusy", err)
				}
				r.release()
				r.finish(check)
			},
		},
		{
			"the first read-back after a missed save fails its transfer, and the check goes on",
			func(r *saveRig, check *blink1PatternCheck) {
				r.stepToSave(check)
				r.saveHeld(check)
				r.release()
				r.device.mu.Lock()
				r.device.readErrs = 1
				r.device.mu.Unlock()
				if _, err := r.try(check); !errors.Is(err, unix.EIO) {
					r.t.Fatalf("control: the first read-back with its transfer failing = %v, want EIO", err)
				}
				r.finish(check)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSaveRig(t, "save-gap")
			light := r.open()
			check := r.check(light)
			tc.run(r, check)
			r.requireNotFalseBlack(light, tc.name)
		})
	}
}
