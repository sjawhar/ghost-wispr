//go:build linux && (amd64 || arm64)

package indicator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// stallLight is a blink(1) for the stall test: the LED's colour, when it last
// went off, and each watchdog tickle.
type stallLight struct {
	mu      sync.Mutex
	colour  [3]byte
	offAt   time.Time
	tickles []time.Time
}

func (l *stallLight) ioctl(_ uintptr, request uintptr, report *[8]byte) error {
	time.Sleep(2 * time.Millisecond) // a USB control transfer
	l.mu.Lock()
	defer l.mu.Unlock()
	if request == hidIOCGetFeature {
		if report[1] == 'r' {
			report[2], report[3], report[4] = l.colour[0], l.colour[1], l.colour[2]
		}
		return nil
	}
	switch report[1] {
	case 'n':
		colour := [3]byte{report[2], report[3], report[4]}
		if colour == [3]byte{} && l.colour != colour {
			l.offAt = time.Now()
		}
		l.colour = colour
	case 'D':
		if report[2] == 1 {
			l.tickles = append(l.tickles, time.Now())
		}
	}
	return nil
}

// TestAPauseTurnsTheLightOffWithin500msWhileAnotherOpenStallsTheLock fails if
// a pause leaves the light red for more than 500 ms, the bound the README
// gives, while another open of the node holds the lock and does not let go
// for 6 s (a `ghost-wispr light-state` stopped inside its exchange, or any
// member of plugdev running `flock /dev/hidrawN sleep 6`); or if the watchdog
// goes untickled for its timeout meanwhile. The recorder's handle reads the
// light back every 250 ms meanwhile, as the gate's writer does for each buffer
// of audio.
func TestAPauseTurnsTheLightOffWithin500msWhileAnotherOpenStallsTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidraw0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := func() *os.File {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		return file
	}
	recorder := &blink1{file: open(), patternLines: 32}
	holder, probe := open(), open()
	light := &stallLight{}
	original := featureIOCTL
	t.Cleanup(func() { featureIOCTL = original })
	featureIOCTL = light.ioctl

	var recording atomic.Bool
	recording.Store(true)
	controller := NewController(recorder, recording.Load, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		controller.Run(ctx)
	}()
	go func() { // the gate's writer: a read-back for each buffer of audio
		defer loops.Done()
		for ctx.Err() == nil {
			_, _, _, _ = recorder.ReadRGB()
			time.Sleep(250 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		cancel()
		loops.Wait()
	})
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		light.mu.Lock()
		red := light.colour == [3]byte{255, 0, 0}
		light.mu.Unlock()
		if red {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control: the light was not red within 3 s of the recording")
		}
	}

	// Another open takes the lock and stalls with it for 6 s; the pause comes
	// at once.
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("control: another open took the held lock: %v", err)
	}
	held := time.Now()
	recording.Store(false)
	controller.Wake()
	time.Sleep(6*time.Second - time.Since(held))
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	light.mu.Lock()
	defer light.mu.Unlock()
	if light.colour != ([3]byte{}) || light.offAt.Before(held) {
		t.Fatalf("the light was %v 6s after a pause while another open held the lock, want off", light.colour)
	}
	if off := light.offAt.Sub(held); off > 500*time.Millisecond {
		t.Fatalf("the light went off %v after a pause while another open held the lock, want within 500ms", off.Round(time.Millisecond))
	}
	var worst time.Duration
	for i := 1; i < len(light.tickles); i++ {
		worst = max(worst, light.tickles[i].Sub(light.tickles[i-1]))
	}
	worst = max(worst, time.Since(light.tickles[len(light.tickles)-1]))
	if worst > serverDownTimeout {
		t.Fatalf("no watchdog tickle for %v while another open held the lock, want one within each %v", worst.Round(10*time.Millisecond), serverDownTimeout)
	}
	t.Logf("off %v after the pause; the longest time between two tickles %v", light.offAt.Sub(held).Round(time.Millisecond), worst.Round(time.Millisecond))
}
