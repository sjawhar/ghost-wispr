//go:build linux && (amd64 || arm64)

package indicator

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// sharedBlink1 is one blink(1) as two open handles see it: a single feature
// report buffer. A SET_FEATURE runs the command; GET_FEATURE returns whatever
// the last SET left in the buffer, whoever sent it. On the office Pi
// (b606ab7, 2026-11-18, usbmon) a GET after the recorder's tickle ('D')
// returned `01440101 2c01001f`, its echo, and a GET after a version query ('v')
// returned `01760033 33000000`: a blink(1) mk3.
//
// Its LEDs and colour commands are the mk3 firmware's (todbot/blink1mk3,
// firmware/firmware-v30x/main.c, the same in tags firmware-v303 and
// firmware-v304), or with mk2 the mk2 firmware's (todbot/blink1,
// hardware/firmware_mk2/main.c):
//
//   - leds[0] is LED 1, leds[1] LED 2; the mk3 keeps 18, of which the rest
//     drive nothing.
//   - ledn is the firmware's global LED number. 'l' sets it, and so does each
//     line the pattern player plays.
//   - 'r' reads leds[ledn] on an mk3, whatever LED its request names; on an
//     mk2 it reads the LED byte 7 names (0 and 1 both read LED 1).
//   - 'n' with byte 7 = 0 writes every LED. With byte 7 = n > 0 it writes
//     leds[n], one LED past the one n names, and starts the pattern player
//     (playing = PLAY_DIRECTLED): the stored pattern, which Ghost Wispr keeps
//     black, fades every LED to black, and its lines set ledn to 0.
//   - 'c' with byte 7 = 0 fades every LED; with byte 7 = n > 0 it fades
//     leds[n-1] only. The fade ends within two of the firmware's 10 ms ticks
//     for the fade of 1 tick Ghost Wispr sends; this fake ends it at once.
type sharedBlink1 struct {
	mu     sync.Mutex
	mk2    bool
	leds   [18][3]byte
	ledn   byte
	buffer [8]byte
	names  map[uintptr]string
	// between runs once, outside the lock, right after the SET_FEATURE of
	// command cmd from handle who; it may run the other handle's command.
	between     func()
	betweenWho  string
	betweenCmd  byte
	betweenDone bool
	setFrom     map[string]chan struct{}
}

func (d *sharedBlink1) ioctl(fd uintptr, request uintptr, report *[8]byte) error {
	d.mu.Lock()
	who := d.names[fd]
	if request == hidIOCGetFeature {
		*report = d.buffer
		d.mu.Unlock()
		return nil
	}
	cmd := report[1]
	colour := [3]byte{report[2], report[3], report[4]}
	n := report[7]
	d.buffer = *report
	switch cmd {
	case 'n':
		if n == 0 {
			for i := range d.leds {
				d.leds[i] = colour
			}
			break
		}
		d.leds[n] = colour
		d.leds, d.ledn = [18][3]byte{}, 0 // the black pattern plays
	case 'c':
		if n == 0 {
			for i := range d.leds {
				d.leds[i] = colour
			}
			break
		}
		d.leds[n-1] = colour
	case 'l':
		d.ledn = report[2]
	case 'r':
		led := d.ledn
		if d.mk2 {
			led = max(n, 1) - 1
		}
		d.buffer = [8]byte{0x01, 'r', d.leds[led][0], d.leds[led][1], d.leds[led][2], 0, 0, max(n, 1) - 1}
	case 'v':
		d.buffer = [8]byte{0x01, 'v', 0, '3', '3'}
	}
	if ch := d.setFrom[who]; ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	run := d.between != nil && !d.betweenDone && who == d.betweenWho && cmd == d.betweenCmd
	if run {
		d.betweenDone = true
	}
	d.mu.Unlock()
	if run {
		d.between()
	}
	return nil
}

// led returns LED i's colour: 0 is LED 1, 1 is LED 2.
func (d *sharedBlink1) led(i int) [3]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leds[i]
}

// openTwice opens one fake hidraw node twice, as the recorder and a second
// process (`ghost-wispr light-state`) each do.
func openTwice(t *testing.T, device *sharedBlink1) (first, second *blink1) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hidraw0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := func(name string) *blink1 {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		device.names[file.Fd()] = name
		return &blink1{file: file, patternLines: 32}
	}
	return open("first"), open("second")
}

// TestReadRGBIsNotAnsweredByAnotherHandlesCommand fails if a read of the
// blink(1)'s colour returns the reply to a command another handle sent between
// the read's SET_FEATURE and its GET_FEATURE. On the office Pi (b606ab7,
// 2026-11-18 02:08Z) two handles were open on hidraw0: the recorder's and a
// `ghost-wispr light-state` the operator ran while it recorded, as the runbook
// tells them to. usbmon:
//
//	02:08:02.530Z S Co:3:002:0 … = 01720000 00000000   (a read: 'r')
//	02:08:02.530Z S Co:3:002:0 … = 01760000 00000000   (the other handle's version query: 'v')
//	02:08:02.541Z C Ci:3:002:0 0 8 = 01760033 33000000
//	02:08:02.545Z C Ci:3:002:0 0 8 = 01760033 33000000
//
// The recorder read the red light as 0,51,51, counted it as not red, dropped the
// audio and reopened the microphone ("the recording light was not red while the
// microphone was open; reopening the microphone"). The other way round, at
// 02:08:50.776Z, `light-state` read the recorder's tickle as the colour 1,1,44
// and printed "recording light is neither red nor off". A read must return the
// colour whatever another handle sends meanwhile.
func TestReadRGBIsNotAnsweredByAnotherHandlesCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		other   func(*blink1) error
		otherOp string
	}{
		{"a version query from a second process (light-state opening)", func(b *blink1) error { return b.loadVersion() }, "v"},
		{"the recorder's watchdog tickle", func(b *blink1) error { return b.ServerDown(3 * time.Second) }, "D"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := &sharedBlink1{names: map[uintptr]string{}, setFrom: map[string]chan struct{}{"second": make(chan struct{}, 1)}}
			original, originalWait := featureIOCTL, exchangeWait
			featureIOCTL = device.ioctl
			// The other handle's command waits for the lock past the 100 ms
			// this test holds the read between its SET and GET: a lone SET
			// whose wait ran out goes without the lock (sendLone).
			exchangeWait = time.Second
			t.Cleanup(func() { featureIOCTL, exchangeWait = original, originalWait })
			reader, other := openTwice(t, device)
			if err := reader.SetRGB(255, 0, 0); err != nil {
				t.Fatal(err)
			}

			// Control: with nothing in between, the fake reads red back.
			if r, g, b, err := reader.ReadRGB(); err != nil || r != 255 || g != 0 || b != 0 {
				t.Fatalf("control: ReadRGB = %d,%d,%d, %v, want 255,0,0", r, g, b, err)
			}

			// The other handle's command, sent right after the read's SET_FEATURE.
			// A lock held across SET and GET makes it wait; give it 100 ms to land.
			otherDone := make(chan error, 1)
			device.mu.Lock()
			device.betweenWho, device.betweenCmd, device.betweenDone = "first", 'r', false
			device.between = func() {
				go func() { otherDone <- tc.other(other) }()
				select {
				case <-device.setFrom["second"]:
				case <-time.After(100 * time.Millisecond):
				}
			}
			device.mu.Unlock()

			r, g, b, err := reader.ReadRGB()
			if err := <-otherDone; err != nil {
				t.Fatalf("the other handle's %s: %v", tc.otherOp, err)
			}
			if err != nil {
				t.Fatalf("ReadRGB: %v", err)
			}
			if r != 255 || g != 0 || b != 0 {
				t.Fatalf("ReadRGB of a red light = %d,%d,%d with the other handle's %q between its SET and GET, want 255,0,0", r, g, b, tc.otherOp)
			}
		})
	}
}
