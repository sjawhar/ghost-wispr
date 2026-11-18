//go:build linux && (amd64 || arm64)

package indicator

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// slowQueryBlink1 is a blink(1) whose one report buffer answers a GET with
// what the last SET left, and whose pattern line queries take slow between
// their SET and GET, longer than the recorder's 50 ms wait for the light.
// hook runs once, inside the SET of the query for pattern line 2. log records
// each transfer in order.
type slowQueryBlink1 struct {
	slow time.Duration
	hook func()

	mu     sync.Mutex
	buffer [8]byte
	colour [3]byte
	ran    bool
	log    []string
}

func (d *slowQueryBlink1) ioctl(_ uintptr, request uintptr, report *[8]byte) error {
	d.mu.Lock()
	if request == hidIOCGetFeature {
		*report = d.buffer
		d.log = append(d.log, "GET "+string(report[1]))
		d.mu.Unlock()
		return nil
	}
	d.log = append(d.log, "SET "+string(report[1]))
	switch report[1] {
	case 'R':
		d.buffer = [8]byte{0x01, 'R', 0, 0, 0, 0, 10, report[7]} // a black line with a duration
	case 'r':
		d.buffer = [8]byte{0x01, 'r', d.colour[0], d.colour[1], d.colour[2]}
	case 'n':
		copy(d.colour[:], report[2:5])
		d.buffer = *report
	default:
		d.buffer = *report
	}
	slow := report[1] == 'R'
	run := d.hook != nil && !d.ran && slow && report[7] == 2
	if run {
		d.ran = true
	}
	d.mu.Unlock()
	if run {
		d.hook()
	}
	if slow {
		time.Sleep(d.slow)
	}
	return nil
}

func (d *slowQueryBlink1) entries() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...)
}

// sameHandle opens a recorder's handle on a node no other open holds, with
// device as the blink(1).
func sameHandle(t *testing.T, device *slowQueryBlink1) *blink1 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hidraw0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	original := featureIOCTL
	featureIOCTL = device.ioctl
	t.Cleanup(func() { featureIOCTL = original })
	return &blink1{file: file, patternLines: 32}
}

// TestAHandlesReadWaitsForItsOwnPatternCheck fails if a read on a handle
// whose pattern check is in flight gets ErrBusy, which says another open of
// the node held its lock, when no other open did: each of the check's queries
// holds the handle longer than the read's 50 ms wait (review 5956273705 of
// #79: 3 of 6 reads busy). A command of the handle waits for the handle's own
// exchange; only another open's lock is bounded.
func TestAHandlesReadWaitsForItsOwnPatternCheck(t *testing.T) {
	device := &slowQueryBlink1{slow: 60 * time.Millisecond, colour: [3]byte{255, 0, 0}}
	light := sameHandle(t, device)
	if r, g, b, err := light.ReadRGB(); err != nil || r != 255 || g != 0 || b != 0 {
		t.Fatalf("control: a read with nothing else on the handle = %d,%d,%d, %v", r, g, b, err)
	}

	checked := make(chan error, 1)
	go func() { checked <- light.BlackPattern() }()
	reads, busy := 0, 0
	for done := false; !done; {
		select {
		case err := <-checked:
			if err != nil {
				t.Fatalf("control: the pattern check: %v", err)
			}
			done = true
		default:
			r, g, b, err := light.ReadRGB()
			reads++
			switch {
			case errors.Is(err, ErrBusy):
				busy++
			case err != nil:
				t.Fatalf("read during the pattern check: %v", err)
			case r != 255 || g != 0 || b != 0:
				t.Fatalf("read during the pattern check = %d,%d,%d, want 255,0,0", r, g, b)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if reads < 5 {
		t.Fatalf("control: %d reads ran during the pattern check, want at least 5", reads)
	}
	if busy > 0 {
		t.Fatalf("%d of %d reads on the handle got ErrBusy while its own pattern check ran, though no other open held the node's lock", busy, reads)
	}
}

// TestAnOffWriteDoesNotLandInsideItsHandlesOwnExchange fails if an off write,
// a lone SET that goes without the node's lock once its wait runs out, is
// sent inside an exchange of its own handle: its echo then answers that
// exchange's query, and the pattern check fails with ErrOtherReply (review
// 5956273705 of #79: 'n' answered 'R'). The write must go after that
// exchange's GET, and the check pass.
func TestAnOffWriteDoesNotLandInsideItsHandlesOwnExchange(t *testing.T) {
	device := &slowQueryBlink1{slow: 60 * time.Millisecond, colour: [3]byte{255, 0, 0}}
	light := sameHandle(t, device)
	wrote := make(chan error, 1)
	device.hook = func() { go func() { wrote <- light.SetRGB(0, 0, 0) }() }

	if err := light.BlackPattern(); err != nil {
		t.Fatalf("the pattern check with an off write sent during one of its exchanges: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the off write: %v", err)
	}
	device.mu.Lock()
	colour := device.colour
	device.mu.Unlock()
	if colour != ([3]byte{}) {
		t.Fatalf("LED after the off write = %v, want off", colour)
	}
	log := device.entries()
	write := -1
	for i, entry := range log {
		if entry == "SET n" {
			write = i
		}
	}
	if write < 0 {
		t.Fatalf("control: the transfers %v have no off write", log)
	}
	if write == 0 || log[write-1] != "GET R" {
		t.Fatalf("the off write went at transfer %d, after %q: inside an exchange of its own handle (transfers %v)", write, log[max(write-1, 0)], log[max(write-3, 0):min(write+2, len(log))])
	}
}
