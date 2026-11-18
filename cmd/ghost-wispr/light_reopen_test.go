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
// another command gets in between two of its 33 queries, and checkTakes is how
// long it takes when it gets the light. known is what the driver reports for a
// unit this process checked black before, by its serial number.
type heldLight struct {
	*reconnectLightFake
	held          *atomic.Bool
	known         bool
	checkBusy     atomic.Bool
	checkTakes    time.Duration
	patternChecks atomic.Int32
	black         atomic.Bool
	armedUnknown  atomic.Int32 // tickles that armed the watchdog before the pattern was known black
}

var errHeld = fmt.Errorf("read blink(1) version: %w", indicator.ErrBusy)

func (l *heldLight) PatternKnownBlack() bool { return l.known }

func (l *heldLight) BlackPattern() error {
	if l.held.Load() || l.checkBusy.Load() {
		return errHeld
	}
	time.Sleep(l.checkTakes)
	l.patternChecks.Add(1)
	l.black.Store(true)
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

func (l *heldLight) ServerDown(timeout time.Duration) error {
	if timeout > 0 && !l.known && !l.black.Load() {
		l.armedUnknown.Add(1)
	}
	return l.reconnectLightFake.ServerDown(timeout)
}

func (l *heldLight) colour() [3]uint8 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reconnectLightFake.colour
}

func (l *heldLight) tickleCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tickles
}

// failTheRecordersHandle makes the recorder's handle on the light fail with a
// device error, which closes it, and returns the light it opens again: the
// same unit when known, else another one, as after an unplug.
func failTheRecordersHandle(t *testing.T, held *atomic.Bool, known bool) (*reconnectingLight, *heldLight) {
	t.Helper()
	first := &reconnectLightFake{colour: [3]uint8{255, 0, 0}}
	second := &heldLight{reconnectLightFake: &reconnectLightFake{colour: [3]uint8{255, 0, 0}}, held: held, known: known}
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

// TestAKnownLightOpenedAgainWhileHeldIsTickled fails if, after a device error
// closed the recorder's handle, another open that holds the lock while the
// same unit is opened again keeps its watchdog untickled past its 3 s timeout,
// or the light on more than 500 ms after a pause. The unit is known black (the
// driver checked its serial number before), so it needs no check (review
// 5953011789 and 5954543056 of #79). The lock is held once, for the reopen
// alone, and for 3.5 s without a break, longer than a tickle's timeout.
func TestAKnownLightOpenedAgainWhileHeldIsTickled(t *testing.T) {
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
			light, second := failTheRecordersHandle(t, &held, true)

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
				if second.tickleCount() > 0 {
					if tickled < 0 {
						tickled = time.Since(lost)
					}
					longest = max(longest, time.Since(last))
					last = time.Now()
				}
				if off < 0 && second.colour() == ([3]uint8{}) {
					off = time.Since(lost)
				}
			}
			if tickled < 0 || tickled > 500*time.Millisecond || longest > tickleTimeout || off < 0 || off > 500*time.Millisecond {
				t.Fatalf("after a device error, with another open holding the lock %v: first tickle after %v, longest time between tickles %v, first off write after %v (-1ns: never); each tickle sets a %v timeout, and a pause turns the light off within 500ms",
					tc.hold, tickled, longest, off, tickleTimeout)
			}
			if n := second.patternChecks.Load(); n != 0 {
				t.Fatalf("pattern checks of a unit known black = %d, want 0", n)
			}
		})
	}
}

// TestAnotherUnitIsNotArmedUntilItsPatternIsChecked fails if the recorder arms
// the watchdog of a blink(1) whose stored pattern it has not found black: a
// unit plugged in after an unplug, opened while another open held the lock for
// its black pattern check. If no tickle followed (a crash, kill -STOP), that
// watchdog would play the unit's own pattern, not black (red team, review
// 5954543056 of #79). The unit still gets off writes, and no red, and once a
// red confirmation's check finds it black it is tickled. Control: with the
// lock free at the reopen, the check runs at once and the tickles reach it.
func TestAnotherUnitIsNotArmedUntilItsPatternIsChecked(t *testing.T) {
	for _, tc := range []struct {
		name         string
		heldAtReopen bool
	}{
		{"control: the lock is free at the reopen", false},
		{"another open holds the lock at the reopen", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var held atomic.Bool
			light, second := failTheRecordersHandle(t, &held, false)
			held.Store(tc.heldAtReopen)
			for range 3 {
				_ = light.ServerDown(3 * time.Second)
			}
			held.Store(false)
			if n := second.armedUnknown.Load(); n > 0 {
				t.Fatalf("%d of 3 watchdog tickles armed a blink(1) whose stored pattern was not found black", n)
			}
			if !tc.heldAtReopen {
				if n := second.tickleCount(); n != 3 {
					t.Fatalf("control: tickles that reached a unit checked at its reopen = %d, want 3", n)
				}
				return
			}
			if n := second.tickleCount(); n != 0 {
				t.Fatalf("tickles that reached the unchecked unit = %d, want 0", n)
			}
			if err := light.SetRGB(0, 0, 0); err != nil || second.colour() != ([3]uint8{}) {
				t.Fatalf("off write to the unchecked unit: %v, colour %v, want it off", err, second.colour())
			}
			if err := light.SetRGB(255, 0, 0); err == nil || second.colour() != ([3]uint8{}) {
				t.Fatalf("red write to the unchecked unit: %v, colour %v, want it refused", err, second.colour())
			}
			if err := light.ConfirmRed(); err == nil {
				t.Fatal("red confirmation of the unchecked unit passed, want it refused")
			}
			if n := second.patternChecks.Load(); n != 0 {
				t.Fatalf("pattern checks run by tickles, writes and a confirmation = %d, want 0: only the check before a red confirmation runs one", n)
			}

			if err := light.CheckPattern(); err != nil {
				t.Fatalf("the pattern check before a red confirmation, with the lock free: %v", err)
			}
			if err := light.ConfirmRed(); err != nil {
				t.Fatalf("red confirmation once the pattern is checked: %v", err)
			}
			if err := light.ServerDown(3 * time.Second); err != nil || second.tickleCount() != 1 {
				t.Fatalf("tickle once the pattern is checked: %v, %d reached the unit, want 1", err, second.tickleCount())
			}
			if n := second.patternChecks.Load(); n != 1 {
				t.Fatalf("pattern checks = %d, want 1", n)
			}
		})
	}
}

// TestAPauseDoesNotWaitForAPatternCheck fails if a pause's off write waits for
// the pattern check of a light the recorder opened again while another open
// held the lock. The check takes 1.5 s, as 33 commands do when another open
// lets each through only after the recorder's 50 ms wait (review 5954543056
// of #79: 1.86 s). It ran after a tickle, in front of the controller's off
// write, and in a red confirmation, whose red hold swallowed the off write; it
// now runs before the hold, without the light's lock.
func TestAPauseDoesNotWaitForAPatternCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check func(*hardwareControl) func() error // what runs the check while the pause comes
	}{
		{"a tickle", func(h *hardwareControl) func() error {
			return func() error { return h.light.ServerDown(3 * time.Second) }
		}},
		{"a red confirmation", func(h *hardwareControl) func() error {
			return func() error {
				err := h.EnsureRed()
				if err == nil {
					h.ReleaseRedHold()
				}
				return err
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var held atomic.Bool
			light, second := failTheRecordersHandle(t, &held, false)
			held.Store(true)
			_, _, _, _ = light.ReadRGB() // the reopen meets the lock
			held.Store(false)
			second.checkTakes = 1500 * time.Millisecond
			second.mu.Lock()
			second.reconnectLightFake.colour = [3]uint8{255, 0, 0} // still red from before the device error
			second.mu.Unlock()
			hardware := &hardwareControl{light: light}
			light.holdRed = hardware.holdsRed

			done := make(chan error, 1)
			go func() { done <- tc.check(hardware)() }()
			time.Sleep(50 * time.Millisecond) // the check is under way
			paused := time.Now()
			_ = light.SetRGB(0, 0, 0) // the controller's off write after the pause
			var off time.Duration = -1
			for deadline := paused.Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if second.colour() == ([3]uint8{}) {
					off = time.Since(paused)
					break
				}
			}
			<-done
			if off < 0 || off > 500*time.Millisecond {
				t.Fatalf("a pause during %s's pattern check turned the light off after %v (-1ns: not within 1s), want within 500ms", tc.name, off)
			}
		})
	}
}
