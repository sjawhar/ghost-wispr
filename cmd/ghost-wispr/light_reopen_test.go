package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/indicator"
)

// heldLight is a blink(1) that another open of its node may hold the lock of
// (held). The driver's queries and red writes then get ErrBusy and send
// nothing; its lone SETs, the watchdog tickle and an off write, still reach
// the light. The black pattern check, which starts with the version query and
// pattern reads, is a query; checkBusy makes it alone meet the lock, as when
// another command gets in between two of its 33 queries.
type heldLight struct {
	*reconnectLightFake
	held          *atomic.Bool
	checkBusy     atomic.Bool
	patternChecks atomic.Int32
}

var errHeld = fmt.Errorf("read blink(1) version: %w", indicator.ErrBusy)

func (l *heldLight) BlackPattern() error {
	if l.held.Load() || l.checkBusy.Load() {
		return errHeld
	}
	l.patternChecks.Add(1)
	return nil
}

func (l *heldLight) ReadRGB() (uint8, uint8, uint8, error) {
	if l.held.Load() {
		return 0, 0, 0, indicator.ErrBusy
	}
	return l.reconnectLightFake.ReadRGB()
}

func (l *heldLight) SetRGB(r, g, b uint8) error {
	if l.held.Load() && (r != 0 || g != 0 || b != 0) {
		return indicator.ErrBusy
	}
	return l.reconnectLightFake.SetRGB(r, g, b)
}

// failTheRecordersHandle makes the recorder's handle on the light fail with a
// device error, which closes it, and returns the light it reopens.
func failTheRecordersHandle(t *testing.T, held *atomic.Bool) (*reconnectingLight, *heldLight) {
	t.Helper()
	first := &reconnectLightFake{colour: [3]uint8{255, 0, 0}}
	second := &heldLight{reconnectLightFake: &reconnectLightFake{colour: [3]uint8{255, 0, 0}}, held: held}
	original := openBlink1
	openBlink1 = func(string, string) (indicator.Light, error) { return second, nil }
	t.Cleanup(func() { openBlink1 = original })
	light := &reconnectingLight{light: first, warnings: newWarningsRegistry(), retryEvery: hardwareRetry}

	first.mu.Lock()
	first.readErr = errors.New("blink(1) feature report: input/output error")
	first.mu.Unlock()
	if _, _, _, err := light.ReadRGB(); err == nil || indicator.Contended(err) {
		t.Fatalf("control: ReadRGB of a failed device = %v, want a device error", err)
	}
	if !first.isClosed() {
		t.Fatal("control: a device error did not close the recorder's handle")
	}
	return light, second
}

// TestAContendedReopenStillTicklesAndTurnsTheLightOff fails if, after a device
// error closed the recorder's handle, another open that holds the lock while
// the light is opened again keeps the watchdog untickled past its 3 s timeout,
// or the light on more than 500 ms after a pause. The reopen's black pattern
// check met the lock and counted as a missing light, so the next reopen came
// 10 s later (hardwareRetry), and no command reached the light meanwhile (red
// team, review 5953011789 of #79). The lock is held once, for the reopen alone,
// and then for 3.5 s without a break, longer than a tickle's timeout.
func TestAContendedReopenStillTicklesAndTurnsTheLightOff(t *testing.T) {
	const tickleTimeout = 3 * time.Second // indicator's serverDownTimeout
	for _, tc := range []struct {
		name string
		hold time.Duration // how long another open holds the lock after the device error
	}{
		{"control: the light is free at the reopen", 0},
		{"another open holds the lock for the reopen alone", time.Nanosecond},
		{"another open holds the lock 3.5s", 3500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var held atomic.Bool
			light, second := failTheRecordersHandle(t, &held)

			held.Store(tc.hold > 0)
			lost := time.Now()
			var release sync.Once
			free := func() { release.Do(func() { held.Store(false) }) }
			t.Cleanup(free)
			if tc.hold == time.Nanosecond {
				_ = light.ServerDown(tickleTimeout) // the reopen
				free()
			} else if tc.hold > 0 {
				time.AfterFunc(tc.hold, free)
			}

			// The controller tickles and, the recording paused, writes the light off.
			var tickled, off time.Duration = -1, -1
			var longest time.Duration
			last := lost
			for deadline := lost.Add(tc.hold + 500*time.Millisecond); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
				_ = light.ServerDown(tickleTimeout)
				_ = light.SetRGB(0, 0, 0)
				second.mu.Lock()
				tickles, colour := second.tickles, second.colour
				second.mu.Unlock()
				if tickles > 0 {
					if tickled < 0 {
						tickled = time.Since(lost)
					}
					longest = max(longest, time.Since(last))
					last = time.Now()
				}
				if off < 0 && colour == ([3]uint8{}) {
					off = time.Since(lost)
				}
			}
			if tickled < 0 || tickled > 500*time.Millisecond || longest > tickleTimeout || off < 0 || off > 500*time.Millisecond {
				t.Fatalf("after a device error, with another open holding the lock %v: first tickle after %v, longest time between tickles %v, first off write after %v (-1ns: never); each tickle sets a %v timeout, and a pause turns the light off within 500ms",
					tc.hold, tickled, longest, off, tickleTimeout)
			}
		})
	}
}

// TestALightReopenedUncheckedLightsNoRed fails if the recorder lights red, or
// confirms red, on a light it opened again while another open held the lock,
// before its black pattern is checked: the watchdog plays that pattern if the
// recorder stops, and only a black one turns the light off. The pattern check
// meets the lock while the red write would not. Once the check gets the light,
// the next red confirmation checks the pattern, once, and passes.
func TestALightReopenedUncheckedLightsNoRed(t *testing.T) {
	var held atomic.Bool
	light, second := failTheRecordersHandle(t, &held)
	held.Store(true)
	if err := light.ServerDown(3 * time.Second); err != nil {
		t.Fatalf("watchdog tickle to a light reopened while another open holds the lock: %v", err)
	}
	held.Store(false)
	second.checkBusy.Store(true)
	second.mu.Lock()
	second.colour = [3]uint8{}
	second.mu.Unlock()

	if err := light.ConfirmRed(); !indicator.Contended(err) {
		t.Fatalf("red confirmation of a light whose pattern is unchecked = %v, want a contended error", err)
	}
	if err := light.SetRGB(255, 0, 0); !indicator.Contended(err) {
		t.Fatalf("red write to a light whose pattern is unchecked = %v, want a contended error", err)
	}
	if second.isRed() {
		t.Fatal("a red write reached a light whose pattern is unchecked")
	}
	if second.isClosed() {
		t.Fatal("a pattern check that met the lock closed the recorder's handle")
	}

	second.checkBusy.Store(false)
	if err := light.ConfirmRed(); err != nil {
		t.Fatalf("red confirmation once the pattern check gets the light: %v", err)
	}
	if n := second.patternChecks.Load(); n != 1 {
		t.Fatalf("black pattern checks before the red confirmation = %d, want 1", n)
	}
	if !second.isRed() {
		t.Fatal("control: the confirmed light is not red")
	}
	if err := light.ConfirmRed(); err != nil {
		t.Fatalf("second red confirmation: %v", err)
	}
	if n := second.patternChecks.Load(); n != 1 {
		t.Fatalf("black pattern checks after the pattern was checked = %d, want still 1", n)
	}
}
