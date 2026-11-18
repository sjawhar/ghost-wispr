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
// returned `01760033 33000000`.
type sharedBlink1 struct {
	mu     sync.Mutex
	colour [3]byte
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
	switch cmd {
	case 'n':
		copy(d.colour[:], report[2:5])
		d.buffer = *report
	case 'r':
		d.buffer = [8]byte{0x01, 'r', d.colour[0], d.colour[1], d.colour[2]}
	case 'v':
		d.buffer = [8]byte{0x01, 'v', 0, '3', '3'}
	default:
		d.buffer = *report
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
