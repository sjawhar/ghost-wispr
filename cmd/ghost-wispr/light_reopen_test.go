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
// long it takes when it gets the light: BlackPattern at once, PatternCheck in
// 33 steps of checkTakes/33. known is what the driver reports for a unit this
// process checked black before, by its serial number.
type heldLight struct {
	*reconnectLightFake
	held          *atomic.Bool
	known         bool
	checkBusy     atomic.Bool
	checkTakes    time.Duration
	patternChecks atomic.Int32
	black         atomic.Bool
	armedUnknown  atomic.Int32 // tickles that armed the watchdog before the pattern was known black
	lastRed       atomic.Int64 // when the last red write reached the light, in Unix nanoseconds; 0 before any
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

func (l *heldLight) PatternCheck() indicator.PatternCheck { return &heldCheck{light: l} }

// heldCheck is heldLight's stepwise check, one command per Step.
type heldCheck struct {
	light *heldLight
	sent  int
}

func (c *heldCheck) Step() (bool, error) {
	if c.light.held.Load() || c.light.checkBusy.Load() {
		return false, errHeld
	}
	time.Sleep(c.light.checkTakes / 33)
	if c.sent++; c.sent < 33 {
		return false, nil
	}
	c.light.patternChecks.Add(1)
	c.light.black.Store(true)
	return true, nil
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
	err := l.reconnectLightFake.SetRGB(r, g, b)
	if err == nil && r == 255 && g == 0 && b == 0 {
		l.lastRed.Store(time.Now().UnixNano())
	}
	return err
}

// redAfter reports whether a red write reached the light after at.
func (l *heldLight) redAfter(at time.Time) bool {
	last := l.lastRed.Load()
	return last != 0 && time.Unix(0, last).After(at)
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
// unit plugged in after an unplug. If no tickle followed (a crash, kill
// -STOP), that watchdog would play the unit's own pattern, not black (red
// team, review 5954543056 of #79). It also fails if the reopen, a tickle or
// a write waits for that check: the unit gets off writes at once and no red,
// and a refused tickle or red write starts the check in the background,
// one command at a time (reviews 5956273705 and 5958478401 of #79). Once the
// check finds the unit black, it is tickled and lit red; the unit is checked
// once. The lock is free, or another open holds it, at the reopen.
func TestAnotherUnitIsNotArmedUntilItsPatternIsChecked(t *testing.T) {
	for _, tc := range []struct {
		name         string
		heldAtReopen bool
	}{
		{"the lock is free at the reopen", false},
		{"another open holds the lock at the reopen", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var held atomic.Bool
			light, second := failTheRecordersHandle(t, &held, false)
			second.checkTakes = 330 * time.Millisecond // 10 ms a command
			held.Store(tc.heldAtReopen)
			started := time.Now()
			for range 3 {
				_ = light.ServerDown(3 * time.Second)
			}
			if err := light.SetRGB(0, 0, 0); err != nil || second.colour() != ([3]uint8{}) {
				t.Fatalf("off write to the unchecked unit: %v, colour %v, want it off", err, second.colour())
			}
			held.Store(false)
			if err := light.SetRGB(255, 0, 0); !errors.Is(err, errLightUnchecked) || second.colour() != ([3]uint8{}) {
				t.Fatalf("red write to the unchecked unit: %v, colour %v, want it refused", err, second.colour())
			}
			if err := light.ConfirmRed(); !errors.Is(err, errLightUnchecked) {
				t.Fatalf("red confirmation of the unchecked unit = %v, want errLightUnchecked", err)
			}
			if took := time.Since(started); took > 100*time.Millisecond {
				t.Fatalf("three tickles, two writes and a confirmation of the unchecked unit took %v: one waited for its check", took)
			}

			if err := light.CheckPattern(); err != nil { // waits for a check the tickles or the red write started
				t.Fatalf("the pattern check before a red confirmation, with the lock free: %v", err)
			}
			if err := light.ConfirmRed(); err != nil {
				t.Fatalf("red confirmation once the pattern is checked: %v", err)
			}
			before := second.tickleCount()
			if err := light.ServerDown(3 * time.Second); err != nil || second.tickleCount() != before+1 {
				t.Fatalf("tickle once the pattern is checked: %v, %d reached the unit, want %d", err, second.tickleCount(), before+1)
			}
			if n := second.armedUnknown.Load(); n > 0 {
				t.Fatalf("%d watchdog tickles armed a blink(1) whose stored pattern was not found black", n)
			}
			if n := second.patternChecks.Load(); n != 1 {
				t.Fatalf("pattern checks = %d, want 1", n)
			}
		})
	}
}

// TestAPauseDuringAReopenTurnsTheLightOffAtOnce fails if a pause while the
// recorder opens the light again after a device error leaves it on more than
// 500 ms. The reopen checked the stored pattern of a unit not known black
// under deviceMu, and the off write waited for the whole check: 1.70 s while
// another open let each of its 33 commands through only after a 50 ms wait
// (red team, review 5956273705 of #79). Controls: a unit known black, whose
// reopen needs no check, and a check with no other open in its way (363 ms).
func TestAPauseDuringAReopenTurnsTheLightOffAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		known bool
		check time.Duration
	}{
		{"control: a unit known black by its serial", true, 33 * 53 * time.Millisecond},
		{"control: no other open slows the check", false, 33 * 11 * time.Millisecond},
		{"another open waits out each of the check's commands", false, 33 * 53 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var held atomic.Bool
			light, second := failTheRecordersHandle(t, &held, tc.known)
			second.checkTakes = tc.check
			reopened := make(chan struct{})
			go func() {
				defer close(reopened)
				_, _, _, _ = light.ReadRGB() // the next command opens the light again
			}()
			t.Cleanup(func() { <-reopened })
			time.Sleep(50 * time.Millisecond)

			paused := time.Now()
			wrote := make(chan struct{})
			go func() {
				defer close(wrote)
				_ = light.SetRGB(0, 0, 0) // the controller's off write after the pause
			}()
			var off time.Duration = -1
			for deadline := paused.Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if second.colour() == ([3]uint8{}) {
					off = time.Since(paused)
					break
				}
			}
			<-wrote
			if off < 0 || off > 500*time.Millisecond {
				t.Fatalf("a pause 50ms into the reopen turned the light off after %v (-1ns: not within 3s), want within 500ms", off)
			}
			<-reopened
			if n := second.patternChecks.Load(); n != 0 {
				t.Fatalf("pattern checks run by the reopen = %d, want 0", n)
			}
		})
	}
}

// TestARedWriteChecksAnUncheckedLightWithoutWaiting fails if the
// controller's red write to a unit it opened again, not known black, never
// gets the light red, or waits for the unit's pattern check. While a
// microphone is open the controller wants red and writes red, and nothing
// else checked the unit, so the unit stayed unchecked and off and capture
// stayed held (TestCaptureResumesWhenTheLightIsRedAgain). The red write now
// starts the check and is refused at once; a pause during the check still
// turns the light off; once the check passes, the controller is woken and
// the next red write lights it.
func TestARedWriteChecksAnUncheckedLightWithoutWaiting(t *testing.T) {
	var held atomic.Bool
	light, second := failTheRecordersHandle(t, &held, false)
	second.checkTakes = 1500 * time.Millisecond
	woken := make(chan struct{}, 1)
	light.checked = func() { woken <- struct{}{} }
	if _, _, _, err := light.ReadRGB(); err != nil { // the reopen
		t.Fatal(err)
	}

	started := time.Now()
	if err := light.SetRGB(255, 0, 0); !errors.Is(err, errLightUnchecked) {
		t.Fatalf("red write to an unchecked unit = %v, want errLightUnchecked", err)
	}
	if took := time.Since(started); took > 100*time.Millisecond {
		t.Fatalf("the red write to an unchecked unit took %v: it waited for the pattern check", took)
	}
	time.Sleep(100 * time.Millisecond) // the check is under way
	paused := time.Now()
	if err := light.SetRGB(0, 0, 0); err != nil || second.colour() != ([3]uint8{}) {
		t.Fatalf("off write during the check: %v, colour %v, want it off", err, second.colour())
	}
	if took := time.Since(paused); took > 500*time.Millisecond {
		t.Fatalf("the off write during the check took %v, want within 500ms", took)
	}
	select {
	case <-woken:
	case <-time.After(3 * time.Second):
		t.Fatalf("the red write started no pattern check that passed within 3s (pattern checks %d)", second.patternChecks.Load())
	}
	if err := light.SetRGB(255, 0, 0); err != nil || !second.isRed() {
		t.Fatalf("red write once the check passed: %v, red %t, want it red", err, second.isRed())
	}
	if n := second.patternChecks.Load(); n != 1 {
		t.Fatalf("pattern checks = %d, want 1", n)
	}
}

// TestAPauseDoesNotWaitForAPatternCheck fails if a pause's off write waits for
// the pattern check of a light the recorder opened again after a device
// error. The check takes 1.5 s, as 33 commands do when another open lets each
// through only after the recorder's 50 ms wait (review 5954543056 of #79:
// 1.86 s). It ran after a tickle, in front of the controller's off write; in
// a red confirmation, whose red hold swallowed the off write; and in the
// controller's check before its first update, under deviceMu throughout. It
// now runs before the red hold, one command per hold of deviceMu.
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
		{"the controller's check before its first update", func(h *hardwareControl) func() error {
			return func() error { return h.light.BlackPattern() }
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
			// The check's own result is not this test's subject: the pause
			// comes during it, so a tickle's or a red confirmation's check
			// reports the pattern not yet checked black. A failure says it.
			checked := <-done
			if off < 0 || off > 500*time.Millisecond {
				t.Fatalf("a pause during %s's pattern check turned the light off after %v (-1ns: not within 1s), want within 500ms (the check returned %v)", tc.name, off, checked)
			}
		})
	}
}
