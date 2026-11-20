//go:build linux && (amd64 || arm64)

package indicator

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// setBlink1Serial gives the fake sysfs's blink(1) the serial number serial
// (HID_UNIQ, as the kernel's usbhid driver sets it from the USB serial
// string), or none when serial is "".
func setBlink1Serial(t *testing.T, sysfsRoot, serial string) {
	t.Helper()
	uevent := "HID_ID=0003:000027B8:000001ED\nHID_NAME=ThingM blink(1) mk3\n"
	if serial != "" {
		uevent += "HID_UNIQ=" + serial + "\n"
	}
	if err := os.WriteFile(filepath.Join(sysfsRoot, "class", "hidraw", "hidraw0", "device", "uevent"), []byte(uevent), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestATickleReachesAKnownBlackLightOpenedWhileHeld fails if the recorder's
// watchdog tickle to a blink(1) that it opened again while another open held
// the lock sends nothing while the lock stays held. The tickle's 'D' report
// needs the light's pattern line count, which an open reads with a locked
// version query; with the lock held the query got ErrBusy, and the tickle read
// it again and sent nothing, so the watchdog fired while the recorder ran
// (review 5954543056 of #79). A unit that this process has found black before
// (the same serial number) is known: it needs no version query and no pattern
// check, and its tickle goes without the lock once its wait runs out. Another
// unit, or one with no serial, is not known: the recorder must not arm its
// watchdog until its own pattern is checked.
func TestATickleReachesAKnownBlackLightOpenedWhileHeld(t *testing.T) {
	for _, tc := range []struct {
		name      string
		checked   string // the serial of the unit this process checked black
		reopened  string // the serial of the unit opened again
		wantKnown bool
	}{
		{name: "the same unit", checked: "20001234", reopened: "20001234", wantKnown: true},
		{name: "control: another unit", checked: "20001235", reopened: "20005678"},
		{name: "control: a unit with no serial", checked: "", reopened: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sysfsRoot, devRoot, node := fakeBlink1Sysfs(t)
			setBlink1Serial(t, sysfsRoot, tc.checked)
			var (
				mu       sync.Mutex
				versions int
				tickles  [][8]byte
			)
			original := featureIOCTL
			t.Cleanup(func() { featureIOCTL = original })
			featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
				mu.Lock()
				defer mu.Unlock()
				if request == hidIOCGetFeature {
					switch report[1] {
					case 'v':
						report[3] = '3'
					case 'R':
						report[6], report[7] = 10, 0 // a black line with a duration, on every LED
					}
					return nil
				}
				switch {
				case report[1] == 'v':
					versions++
				case report[1] == 'D' && report[2] == 1:
					tickles = append(tickles, *report)
				}
				return nil
			}

			first, err := OpenBlink1(sysfsRoot, devRoot)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.BlackPattern(); err != nil {
				t.Fatalf("control: the black pattern check of a free light: %v", err)
			}
			_ = first.Close()

			setBlink1Serial(t, sysfsRoot, tc.reopened)
			holder, err := os.Open(node)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = holder.Close() })
			if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			versions, tickles = 0, nil
			mu.Unlock()
			reopened, err := OpenBlink1(sysfsRoot, devRoot)
			if err != nil {
				t.Fatalf("the reopen while another open holds the lock: %v", err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if known := KnownBlack(reopened); known != tc.wantKnown {
				t.Fatalf("KnownBlack of %s opened again = %t, want %t", tc.name, known, tc.wantKnown)
			}
			if !tc.wantKnown {
				return
			}
			started := time.Now()
			err = reopened.ServerDown(3 * time.Second)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || len(tickles) != 1 {
				t.Fatalf("ServerDown(3s) of a known light opened again while another open holds the lock: %v after %v; tickles that reached the light %d, want 1",
					err, time.Since(started).Round(time.Millisecond), len(tickles))
			}
			if end := tickles[0][7]; end != 31 {
				t.Fatalf("the tickle's end line = %d, want 31 (the mk3's 32 lines)", end)
			}
			if versions != 0 {
				t.Fatalf("version queries sent by the reopen of a known light = %d, want 0", versions)
			}
		})
	}
}
