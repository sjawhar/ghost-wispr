//go:build linux && (amd64 || arm64)

package indicator

import (
	"os"
	"testing"
	"time"
)

// recordSavesInTemp makes the check record its saves in a directory of the
// test's own, as the recorder records them next to its database.
func recordSavesInTemp(t *testing.T) {
	t.Helper()
	RecordSavesIn(t.TempDir())
	t.Cleanup(func() { RecordSavesIn("") })
}

// restart stands for a recorder restarted without a power cycle of the unit:
// the new process knows no unit, and opens the unit again.
func (r *saveRig) restart() (Light, *blink1PatternCheck) {
	r.t.Helper()
	checkedMu.Lock()
	delete(checkedUnits, r.serial)
	checkedMu.Unlock()
	light := r.open()
	return light, r.check(light)
}

// blackUnit makes the fake unit's RAM and flash black, as a unit saved black
// long ago and powered up since.
func (r *saveRig) blackUnit() {
	r.device.mu.Lock()
	defer r.device.mu.Unlock()
	r.device.ram, r.device.flash = [32][3]byte{}, [32][3]byte{}
}

// dateRecords sets the time of each of the unit's records that exists to at.
func (r *saveRig) dateRecords(at time.Time) {
	r.t.Helper()
	for _, kind := range []string{attemptRecord, savedRecord} {
		path := saveRecordPath(r.serial, kind)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := writeSaveRecord(path, at); err != nil {
			r.t.Fatal(err)
		}
	}
}

// recorded reports whether the unit has a record of kind.
func (r *saveRig) recorded(kind string) bool {
	_, err := os.Stat(saveRecordPath(r.serial, kind))
	return err == nil
}

// TestASaveRecordCapsTheFlashWritesOfARestartingRecorder fails if a recorder
// restarted over and over writes the light's flash at each start, or does not
// know the unit black from a confirmed save's record. Each check of a unit not
// known black yet ends in a save, and the recorder's unit restarts every 10 s
// while it fails (Restart=always, RestartSec=10, no start limit): about 8,600
// writes a day. Controls: the first check saves, and records older than
// saveRecordFresh, or from the future, spare nothing.
func TestASaveRecordCapsTheFlashWritesOfARestartingRecorder(t *testing.T) {
	recordSavesInTemp(t)
	r := newSaveRig(t, "record-cap")
	r.blackUnit()
	light, check := r.restart()
	r.finish(check)
	if saves, _, _ := r.state(); saves != 1 || !KnownBlack(light) {
		t.Fatalf("control: the first check of a black unit saved %d times, known %t; want one save and known", saves, KnownBlack(light))
	}
	if !r.recorded(attemptRecord) || !r.recorded(savedRecord) {
		t.Fatalf("control: the first check's save left an attempt record %t and a save record %t, want both", r.recorded(attemptRecord), r.recorded(savedRecord))
	}
	for restart := range 20 {
		light, check := r.restart()
		r.finish(check)
		if saves, _, _ := r.state(); saves != 1 || !KnownBlack(light) {
			t.Fatalf("after %d restarts the unit has %d saves, known %t; want the first check's one save, and the unit known from its record", restart+1, saves, KnownBlack(light))
		}
	}
	for _, stale := range []struct {
		name string
		at   time.Time
	}{
		{"older than saveRecordFresh", time.Now().Add(-saveRecordFresh - time.Minute)},
		{"from the future, as after the clock was set back", time.Now().Add(time.Hour)},
	} {
		before, _, _ := r.state()
		r.dateRecords(stale.at)
		light, check := r.restart()
		r.finish(check)
		if saves, _, _ := r.state(); saves != before+1 || !KnownBlack(light) {
			t.Fatalf("control: with records %s, the check saved %d times, known %t; want one save and known", stale.name, saves-before, KnownBlack(light))
		}
		if !savedRecently(r.serial, time.Now()) {
			t.Fatalf("control: the check that saved with records %s left no fresh save record", stale.name)
		}
	}
}

// TestASaveAttemptWithoutAConfirmedSaveLeavesTheUnitUnknown fails if a save
// that missed the unit is recorded as a save, if a later check within
// saveRecordFresh writes the flash again or knows the unit black, or if a
// save that reached the unit before the read-back showed the written pattern
// black leaves the record of an earlier save. A record that stood for a save
// that did not leave the flash black would let the next process know the unit
// with its flash lit. Control: once the attempt is older than
// saveRecordFresh, a check saves again and knows the unit.
func TestASaveAttemptWithoutAConfirmedSaveLeavesTheUnitUnknown(t *testing.T) {
	t.Run("a save that missed the unit", func(t *testing.T) {
		recordSavesInTemp(t)
		r := newSaveRig(t, "record-missed")
		light := r.open()
		check := r.check(light)
		r.stepToSave(check)
		r.saveHeld(check)
		r.release()
		r.finish(check)
		if !r.recorded(attemptRecord) || r.recorded(savedRecord) {
			t.Fatalf("a save that missed the unit left an attempt record %t and a save record %t; want an attempt and no save", r.recorded(attemptRecord), r.recorded(savedRecord))
		}
		_ = light.Close()
		light, later := r.restart()
		r.finish(later)
		r.requireNotFalseBlack(light, "a new process after the missed save")
		if saves, _, _ := r.state(); saves != 0 || KnownBlack(light) {
			t.Fatalf("a new process within saveRecordFresh of the missed save saved %d times, known %t; want no save and unknown", saves, KnownBlack(light))
		}
		r.dateRecords(time.Now().Add(-saveRecordFresh - time.Minute))
		_ = light.Close()
		light, later = r.restart()
		r.finish(later)
		if saves, flashBlack, _ := r.state(); saves != 1 || !flashBlack || !KnownBlack(light) {
			t.Fatalf("control: once the attempt is older than saveRecordFresh, the check left the unit with %d saves, flash black %t, known %t; want it saved once, black, and known", saves, flashBlack, KnownBlack(light))
		}
	})
	t.Run("a save the read-back does not show black", func(t *testing.T) {
		recordSavesInTemp(t)
		r := newSaveRig(t, "record-unverified")
		r.blackUnit()
		light, check := r.restart()
		r.finish(check)
		if !savedRecently(r.serial, time.Now()) || !KnownBlack(light) {
			t.Fatal("control: the first check of a black unit left no save record, or the unit unknown")
		}
		// Another program lights the unit's RAM, and the unit then takes
		// none of the check's writes. The attempt is old enough to save.
		r.device.mu.Lock()
		for i := range r.device.ram {
			r.device.ram[i] = [3]byte{255, 0, 0}
		}
		r.device.dropWrites = true
		r.device.mu.Unlock()
		r.dateRecords(time.Now().Add(-saveRecordFresh + time.Hour))
		if err := writeSaveRecord(saveRecordPath(r.serial, attemptRecord), time.Now().Add(-saveRecordFresh-time.Minute)); err != nil {
			t.Fatal(err)
		}
		_ = light.Close()
		light, check = r.restart()
		r.stepToSave(check)
		r.step(check) // the save reaches the unit: flash is lit now
		if _, err := r.try(check); err == nil {
			t.Fatal("control: the read-back of a pattern the unit did not take passed")
		}
		if r.recorded(savedRecord) {
			t.Fatal("the record of the earlier save stood after a save of a pattern the read-back did not show black")
		}
		r.requireNotFalseBlack(light, "a save the read-back did not show black")
	})
}
