//go:build linux && (amd64 || arm64)

package indicator

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ledRed   = [3]byte{255, 0, 0}
	ledGreen = [3]byte{0, 255, 0}
	ledOff   = [3]byte{}
)

// firmwares are the two firmwares a blink(1) mk2 or later runs: the office
// Pi's is an mk3's (its version reply is `01760033 33000000`).
var firmwares = []struct {
	name string
	mk2  bool
}{{"mk3", false}, {"mk2", true}}

// installSharedBlink1 makes device answer every feature report until the test
// ends, and opens it twice.
func installSharedBlink1(t *testing.T, device *sharedBlink1) (first, second *blink1) {
	t.Helper()
	if device.names == nil {
		device.names = map[uintptr]string{}
	}
	if device.setFrom == nil {
		device.setFrom = map[string]chan struct{}{}
	}
	original := featureIOCTL
	featureIOCTL = device.ioctl
	t.Cleanup(func() { featureIOCTL = original })
	return openTwice(t, device)
}

// TestReadRGBReadsLED1WhateverLEDTheUnitLastSelected fails if the consent
// check's read returns another LED's colour as LED 1's. An mk3's colour read
// ignores the LED its request names and reads the one its global ledn names,
// which 'l' sets and the pattern player sets from each line it plays; an
// earlier 'l' 1, from another program or a pattern, leaves it on LED 2. The
// recorder must read LED 1 whatever the unit last selected.
func TestReadRGBReadsLED1WhateverLEDTheUnitLastSelected(t *testing.T) {
	for _, fw := range firmwares {
		t.Run(fw.name, func(t *testing.T) {
			device := &sharedBlink1{mk2: fw.mk2}
			reader, _ := installSharedBlink1(t, device)
			device.mu.Lock()
			device.leds[0], device.leds[1], device.ledn = ledRed, ledGreen, 1
			device.mu.Unlock()

			r, g, b, err := reader.ReadRGB()
			if err != nil || [3]byte{r, g, b} != ledRed {
				t.Fatalf("ReadRGB with LED 1 red, LED 2 green and LED 2 selected = %d,%d,%d, %v; want LED 1's 255,0,0", r, g, b, err)
			}
		})
	}
}

// TestReadRGBFailsWhenItCannotSelectLED1 fails if a read whose 'l' 0 did not
// reach the unit still returns a colour: the colour may be another LED's, so
// the read fails, and its colour counts as not red.
func TestReadRGBFailsWhenItCannotSelectLED1(t *testing.T) {
	device := &sharedBlink1{}
	reader, _ := installSharedBlink1(t, device)
	device.mu.Lock()
	device.leds[0] = ledRed
	device.mu.Unlock()
	featureIOCTL = func(fd uintptr, request uintptr, report *[8]byte) error {
		if request == hidIOCSetFeature && report[1] == 'l' {
			return unix.EIO
		}
		return device.ioctl(fd, request, report)
	}

	r, g, b, err := reader.ReadRGB()
	if err == nil || [3]byte{r, g, b} != ledOff {
		t.Fatalf("ReadRGB whose 'l' failed = %d,%d,%d, %v; want no colour and an error", r, g, b, err)
	}
}

// TestSetLED2WritesOnlyTheSecondLED fails if the answer light's write changes
// LED 1, which must stay exactly red while the unit records, or misses LED 2.
// Both firmwares' 'n' with an LED number writes the LED after the one it names
// and starts the pattern player, which fades every LED to the stored black
// pattern; their 'c' with an LED number fades that LED alone.
func TestSetLED2WritesOnlyTheSecondLED(t *testing.T) {
	for _, fw := range firmwares {
		t.Run(fw.name, func(t *testing.T) {
			device := &sharedBlink1{mk2: fw.mk2}
			light, _ := installSharedBlink1(t, device)
			if err := light.SetRGB(255, 0, 0); err != nil {
				t.Fatal(err)
			}
			for _, colour := range [][3]byte{ledGreen, ledOff, ledRed} {
				if err := light.SetLED2(colour[0], colour[1], colour[2]); err != nil {
					t.Fatalf("SetLED2(%v): %v", colour, err)
				}
				if got := device.led(0); got != ledRed {
					t.Fatalf("LED 1 after SetLED2(%v) = %v, want it red as recording left it", colour, got)
				}
				if got := device.led(1); got != colour {
					t.Fatalf("LED 2 after SetLED2(%v) = %v", colour, got)
				}
			}
		})
	}
}

// TestAFlashNeverReadsAsLED1 fails if LED 2's green, or a flash write, a
// recording write, light-off or any command that leaves the unit's global
// ledn on LED 2, can make a read return LED 2's colour as LED 1's. The flash
// writes LED 2 between the gate's reads, and a write of another handle can
// come while a read holds the light, between its 'l' 0 and its 'r'. A write
// that takes the lock waits for the read's GET; light-off goes without the
// lock once its wait runs out (sendLone), and lands between them. Each read
// returns LED 1's colour then, or no colour and an error; never green.
func TestAFlashNeverReadsAsLED1(t *testing.T) {
	t.Run("flash writes between reads", func(t *testing.T) {
		device := &sharedBlink1{}
		light, _ := installSharedBlink1(t, device)
		if err := light.SetRGB(255, 0, 0); err != nil {
			t.Fatal(err)
		}
		for i := range 4 {
			for _, colour := range [][3]byte{ledGreen, ledOff} {
				if err := light.SetLED2(colour[0], colour[1], colour[2]); err != nil {
					t.Fatal(err)
				}
				if r, g, b, err := light.ReadRGB(); err != nil || [3]byte{r, g, b} != ledRed {
					t.Fatalf("read %d after LED 2 went %v = %d,%d,%d, %v; want LED 1's 255,0,0", i, colour, r, g, b, err)
				}
			}
		}
	})

	selectLED2 := func(b *blink1) error {
		report := Blink1Report('l', [6]byte{1})
		return b.send(&report)
	}
	for _, tc := range []struct {
		name  string
		write func(*blink1) error
		wait  time.Duration // the other handle's wait for the lock
	}{
		{"the flash's green", func(b *blink1) error { return b.SetLED2(0, 255, 0) }, time.Second},
		{"a recording write", func(b *blink1) error { return b.SetRGB(255, 0, 0) }, time.Second},
		{"light-off, without the lock", func(b *blink1) error { return b.SetRGB(0, 0, 0) }, 20 * time.Millisecond},
		{"an 'l' 1 that takes the lock", selectLED2, time.Second},
	} {
		t.Run("between a read's 'l' and 'r': "+tc.name, func(t *testing.T) {
			device := &sharedBlink1{setFrom: map[string]chan struct{}{"second": make(chan struct{}, 1)}}
			reader, other := installSharedBlink1(t, device)
			originalWait := exchangeWait
			exchangeWait = tc.wait
			t.Cleanup(func() { exchangeWait = originalWait })
			if err := reader.SetRGB(255, 0, 0); err != nil {
				t.Fatal(err)
			}
			if err := reader.SetLED2(0, 255, 0); err != nil {
				t.Fatal(err)
			}
			device.mu.Lock()
			device.ledn = 1
			device.betweenWho, device.betweenCmd, device.betweenDone = "first", 'l', false
			done := make(chan error, 1)
			device.between = func() {
				go func() { done <- tc.write(other) }()
				select {
				case <-device.setFrom["second"]:
				case <-time.After(100 * time.Millisecond):
				}
			}
			device.mu.Unlock()

			r, g, b, err := reader.ReadRGB()
			if writeErr := <-done; writeErr != nil {
				t.Fatalf("the other handle's write: %v", writeErr)
			}
			if got := [3]byte{r, g, b}; err == nil && got == ledGreen {
				t.Fatalf("ReadRGB = %v with %s between its 'l' and 'r': LED 2's green read as LED 1", got, tc.name)
			}
			if err != nil && !Contended(err) {
				t.Fatalf("ReadRGB with %s between its 'l' and 'r' = %v, want LED 1's colour or a contended read", tc.name, err)
			}
			if err == nil && [3]byte{r, g, b} != device.led(0) && [3]byte{r, g, b} != ledRed {
				t.Fatalf("ReadRGB = %d,%d,%d with %s between its 'l' and 'r', LED 1 is %v", r, g, b, tc.name, device.led(0))
			}

			// The next read, after the write: LED 1's colour, exactly.
			r, g, b, err = reader.ReadRGB()
			if err != nil || [3]byte{r, g, b} != device.led(0) {
				t.Fatalf("ReadRGB after %s = %d,%d,%d, %v; want LED 1's %v", tc.name, r, g, b, err, device.led(0))
			}
			if device.led(0) == ledGreen {
				t.Fatalf("control: LED 1 is green after %s", tc.name)
			}
		})
	}
}
