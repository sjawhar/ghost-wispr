package indicator

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	colourRed   = [3]uint8{255, 0, 0}
	colourGreen = [3]uint8{0, 255, 0}
)

// flashEvery sets waitingFlashEvery to d until the test ends.
func flashEvery(t *testing.T, d time.Duration) {
	t.Helper()
	original := waitingFlashEvery
	waitingFlashEvery = d
	t.Cleanup(func() { waitingFlashEvery = original })
}

// waitingRig runs a controller over a two-LED fakeLight, checking every 5 ms,
// and keeps every check's result.
type waitingRig struct {
	light      *fakeLight
	controller *Controller
	recording  atomic.Bool
	mu         sync.Mutex
	reports    []error
}

func startWaitingRig(t *testing.T, recording bool) *waitingRig {
	t.Helper()
	flashEvery(t, 10*time.Millisecond)
	r := &waitingRig{light: &fakeLight{}}
	r.recording.Store(recording)
	r.controller = NewController(r.light, r.recording.Load, 5*time.Millisecond, func(err error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.reports = append(r.reports, err)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.controller.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	want := [3]uint8{}
	if recording {
		want = colourRed
	}
	waitFor(t, "the first check", func() bool {
		r.light.mu.Lock()
		defer r.light.mu.Unlock()
		return r.light.colour == want && countCalls(r.light.calls, "read") > 0
	})
	return r
}

// led2Writes returns each LED 2 write so far, and LED 1's colour at each read
// and LED 2 write.
func (r *waitingRig) led2Writes() (writes, led1 [][3]uint8) {
	r.light.mu.Lock()
	defer r.light.mu.Unlock()
	return slices.Clone(r.light.led2Writes), slices.Clone(r.light.led1Seen)
}

func (r *waitingRig) leds() (led1, led2 [3]uint8) {
	r.light.mu.Lock()
	defer r.light.mu.Unlock()
	return r.light.colour, r.light.led2
}

// failedChecks returns the checks that reported a failure.
func (r *waitingRig) failedChecks() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var failed []error
	for _, err := range r.reports {
		if err != nil {
			failed = append(failed, err)
		}
	}
	return failed
}

func count(writes [][3]uint8, colour [3]uint8) int {
	n := 0
	for _, w := range writes {
		if w == colour {
			n++
		}
	}
	return n
}

// TestControllerFlashesLED2WhileSomethingWaits fails if LED 2 does not flash,
// green then off, while something waits for a person and the unit records; if
// LED 1 is anything but red throughout, read back and at each LED 2 write, or
// a check during the flash fails; or if LED 2 does not go back to red, and
// stay red, once nothing waits.
func TestControllerFlashesLED2WhileSomethingWaits(t *testing.T) {
	r := startWaitingRig(t, true)
	_, before := r.led2Writes()
	r.controller.SetWaiting(true)
	waitFor(t, "LED 2 green and off twice each", func() bool {
		writes, _ := r.led2Writes()
		return count(writes, colourGreen) >= 2 && count(writes, [3]uint8{}) >= 2
	})
	writes, led1 := r.led2Writes()
	if writes[0] != colourGreen {
		t.Fatalf("LED 2's first write = %v, want green: %v", writes[0], writes)
	}
	for i, colour := range led1[len(before):] {
		if colour != colourRed {
			t.Fatalf("LED 1 was %v at its read or LED 2 write %d during the flash, want exactly red: %v", colour, i, led1[len(before):])
		}
	}
	if failed := r.failedChecks(); len(failed) != 0 {
		t.Fatalf("checks during the flash failed: %v", failed)
	}

	r.controller.SetWaiting(false)
	waitFor(t, "LED 2 back to red", func() bool { _, led2 := r.leds(); return led2 == colourRed })
	stopped, _ := r.led2Writes()
	time.Sleep(10 * waitingFlashEvery)
	if after, _ := r.led2Writes(); len(after) != len(stopped) {
		t.Fatalf("LED 2 written %v after nothing waited, want no write after its red", after[len(stopped):])
	}
	if led1, led2 := r.leds(); led1 != colourRed || led2 != colourRed {
		t.Fatalf("LEDs after the flash = %v, %v; want both red", led1, led2)
	}
}

// TestControllerNeverFlashesWhileNotRecording fails if something waiting
// while the unit does not record writes LED 2 at all.
func TestControllerNeverFlashesWhileNotRecording(t *testing.T) {
	r := startWaitingRig(t, false)
	r.controller.SetWaiting(true)
	time.Sleep(10 * waitingFlashEvery)
	if writes, _ := r.led2Writes(); len(writes) != 0 {
		t.Fatalf("LED 2 written %v while not recording, want nothing", writes)
	}
	if led1, led2 := r.leds(); led1 != ([3]uint8{}) || led2 != ([3]uint8{}) {
		t.Fatalf("LEDs while not recording = %v, %v; want both off", led1, led2)
	}
}

// TestControllerStopsTheFlashAtAPause fails if LED 2 is written after
// recording stops while something still waits: the off write turns both LEDs
// off, and nothing lights LED 2 again until the unit records.
func TestControllerStopsTheFlashAtAPause(t *testing.T) {
	r := startWaitingRig(t, true)
	r.controller.SetWaiting(true)
	waitFor(t, "the flash", func() bool { writes, _ := r.led2Writes(); return len(writes) >= 2 })
	r.recording.Store(false)
	r.controller.Wake()
	waitFor(t, "both LEDs off", func() bool { led1, led2 := r.leds(); return led1 == [3]uint8{} && led2 == [3]uint8{} })
	paused, _ := r.led2Writes()
	time.Sleep(10 * waitingFlashEvery)
	if after, _ := r.led2Writes(); len(after) != len(paused) {
		t.Fatalf("LED 2 written %v after the pause, want nothing", after[len(paused):])
	}
	if led1, led2 := r.leds(); led1 != ([3]uint8{}) || led2 != ([3]uint8{}) {
		t.Fatalf("LEDs after the pause = %v, %v; want both off", led1, led2)
	}

	// Recording again with something still waiting: the flash comes back.
	r.recording.Store(true)
	r.controller.Wake()
	waitFor(t, "the flash again", func() bool {
		writes, _ := r.led2Writes()
		return count(writes[len(paused):], colourGreen) >= 1
	})
}

// TestControllerStopsTheFlashWhenItStops fails if the controller's end leaves
// either LED lit, or writes LED 2 after its off write.
func TestControllerStopsTheFlashWhenItStops(t *testing.T) {
	flashEvery(t, 10*time.Millisecond)
	light := &fakeLight{}
	controller := NewController(light, func() bool { return true }, 5*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { controller.Run(ctx); close(done) }()
	controller.SetWaiting(true)
	waitFor(t, "the flash", func() bool {
		light.mu.Lock()
		defer light.mu.Unlock()
		return len(light.led2Writes) >= 2
	})
	cancel()
	<-done
	calls, _ := light.snapshot()
	if len(calls) < 3 || !slices.Equal(calls[len(calls)-3:], []string{"set", "serverdown", "close"}) {
		t.Fatalf("calls at the end = %v, want a final set, serverdown, close", calls)
	}
	light.mu.Lock()
	defer light.mu.Unlock()
	if light.colour != ([3]uint8{}) || light.led2 != ([3]uint8{}) {
		t.Fatalf("LEDs after the controller ended = %v, %v; want both off", light.colour, light.led2)
	}
}
