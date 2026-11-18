package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/indicator"
)

// gateOnReopenedLight wires capture to the recorder's light as the recorder
// does (wireLightConsent), after a device error made the light open again:
// the same unit when known, else another one whose pattern check takes check.
// The light is off, and no light controller runs.
func gateOnReopenedLight(t *testing.T, known bool, check time.Duration) (*captureGate, *heldLight) {
	t.Helper()
	var held atomic.Bool
	light, second := failTheRecordersHandle(t, &held, known)
	second.checkTakes = check
	if _, _, _, err := light.ReadRGB(); err != nil { // the reopen
		t.Fatalf("control: the reopen after the device error: %v", err)
	}
	second.mu.Lock()
	second.reconnectLightFake.colour = [3]uint8{}
	second.mu.Unlock()
	hardware := &hardwareControl{light: light}
	light.holdRed = hardware.holdsRed
	capture := newCaptureGate(nil, nil, nil)
	wireLightConsent(capture, hardware, newWarningsRegistry(), func(error) {})
	return capture, second
}

// gateChecks are the pattern checks a resume or a light release may run: a
// unit known black needs none (the control); a check of 33 commands of 11 ms;
// a new unit's check that writes its pattern, 68 commands of 11 ms; and a
// check whose 33 commands each wait out another open's 50 ms hold.
var gateChecks = []struct {
	name  string
	known bool
	check time.Duration
}{
	{"control: a unit known black needs no check", true, 33 * 53 * time.Millisecond},
	{"a check of 33 commands of 11 ms", false, 33 * 11 * time.Millisecond},
	{"a new unit's check that writes its pattern, 68 commands of 11 ms", false, 68 * 11 * time.Millisecond},
	{"another open waits out each of the check's 33 commands", false, 33 * 53 * time.Millisecond},
}

// TestAPauseDuringAResumesPatternCheckCancelsTheResume fails if a pause
// tapped while a person's resume checks a unit's stored pattern waits more
// than 500 ms, or if the light turns red after the pause. The resume ran the
// check in its red confirmation, with capture.control held, so the pause
// waited for the whole check and the resume then lit the light (review
// 5958478401 of #79: 1.65 s with another open in the way). Control: with no
// pause, the resume lights the unit once it is checked.
func TestAPauseDuringAResumesPatternCheckCancelsTheResume(t *testing.T) {
	for _, tc := range gateChecks {
		t.Run(tc.name, func(t *testing.T) {
			capture, second := gateOnReopenedLight(t, tc.known, tc.check)
			capture.Pause()
			resumed := make(chan error, 1)
			go func() { resumed <- capture.RequestResume() }()
			time.Sleep(50 * time.Millisecond) // the person taps resume, then pause

			tapped := time.Now()
			capture.Pause()
			if took := time.Since(tapped); took > 500*time.Millisecond {
				t.Fatalf("a pause tapped during the resume's pattern check waited %v, want within 500ms", took.Round(time.Millisecond))
			}
			if err := <-resumed; err != nil {
				t.Fatalf("the resume: %v", err)
			}
			time.Sleep(300 * time.Millisecond) // a gate opening the resume started, if any
			if !capture.IsPaused() {
				t.Fatal("capture is not paused after the pause")
			}
			if second.redAfter(tapped) {
				t.Fatal("the light turned red after the person tapped pause")
			}
		})
	}
	t.Run("control: no pause, the resume lights the unit once it is checked", func(t *testing.T) {
		capture, second := gateOnReopenedLight(t, false, 33*11*time.Millisecond)
		capture.Pause()
		if err := capture.RequestResume(); err != nil {
			t.Fatalf("the resume: %v", err)
		}
		waitForHardware(t, "the gate open and the light red after the resume", func() bool {
			return !capture.isClosed() && second.isRed()
		})
		if n := second.patternChecks.Load(); n != 1 {
			t.Fatalf("pattern checks = %d, want 1", n)
		}
	})
}

// TestAPauseDuringALightReleasesPatternCheckKeepsTheLightOff fails if a pause
// tapped while the gate opens after a light hold (ReleaseLightHold) waits more
// than 500 ms for the unit's pattern check, or if the light turns red after
// the pause. The gate opening ran the check in beforeGateOpen, with
// capture.control held (review 5958478401 of #79). Control: with no pause,
// the gate opens and the light is red.
func TestAPauseDuringALightReleasesPatternCheckKeepsTheLightOff(t *testing.T) {
	for _, tc := range gateChecks {
		t.Run(tc.name, func(t *testing.T) {
			capture, second := gateOnReopenedLight(t, tc.known, tc.check)
			capture.HoldForLight()
			capture.ReleaseLightHold()
			time.Sleep(50 * time.Millisecond) // the gate opening is under way

			tapped := time.Now()
			capture.Pause()
			if took := time.Since(tapped); took > 500*time.Millisecond {
				t.Fatalf("a pause tapped while the gate opens waited %v, want within 500ms", took.Round(time.Millisecond))
			}
			time.Sleep(tc.check + 300*time.Millisecond) // the gate opening ends
			if !capture.IsPaused() || !capture.isClosed() {
				t.Fatalf("after the pause: paused %t, gate closed %t; want both", capture.IsPaused(), capture.isClosed())
			}
			if second.redAfter(tapped) {
				t.Fatal("the light turned red after the person tapped pause")
			}
		})
	}
	t.Run("control: no pause, the gate opens with the light red", func(t *testing.T) {
		capture, second := gateOnReopenedLight(t, false, 33*11*time.Millisecond)
		capture.HoldForLight()
		capture.ReleaseLightHold()
		waitForHardware(t, "the gate open and the light red after the release", func() bool {
			return !capture.isClosed() && second.isRed()
		})
	})
}

// TestACheckStepOfAReplacedLightSendsNothing fails if a step of a pattern
// check that began on one light sends a command, or marks a light checked,
// once the recorder has opened another light: after a device error the old
// check's steps belong to the closed handle, and the new unit needs its own
// check (review 5958478401 of #79).
func TestACheckStepOfAReplacedLightSendsNothing(t *testing.T) {
	var held atomic.Bool
	closed := &heldLight{reconnectLightFake: &reconnectLightFake{}, held: &held}
	current := &heldLight{reconnectLightFake: &reconnectLightFake{}, held: &held}
	light := &reconnectingLight{light: current, patternUnchecked: true, warnings: newWarningsRegistry()}

	check := indicator.NewPatternCheck(closed)
	for step := range 40 {
		done, err := light.checkStep(closed, check)
		if err != nil {
			if !errors.Is(err, errLightUnchecked) {
				t.Fatalf("step %d of the replaced light's check: %v, want errLightUnchecked", step+1, err)
			}
			break
		}
		if done {
			break
		}
	}
	if sent := check.(*heldCheck).sent; sent != 0 {
		t.Fatalf("the replaced light's check sent %d commands after another light was opened, want 0", sent)
	}
	light.deviceMu.Lock()
	unchecked := light.patternUnchecked
	light.deviceMu.Unlock()
	if !unchecked {
		t.Fatal("the replaced light's check marked the current light checked, though nothing checked it")
	}

	// Control: the current light's own check steps.
	own := indicator.NewPatternCheck(current)
	if done, err := light.checkStep(current, own); err != nil || done || own.(*heldCheck).sent != 1 {
		t.Fatalf("control: a step of the current light's check = %t, %v, sent %d; want one command sent", done, err, own.(*heldCheck).sent)
	}
}

// TestARefusedTickleChecksAnUncheckedLight fails if the controller's
// watchdog tickle to a unit it opened again, not known black, starts no
// check of the unit's pattern, or waits for one. A unit that still reads red
// from before the device error needs no red write, so the red write's check
// never started, and the unit stayed unchecked and untickled: on a uhid
// blink(1) the replacement was not red again within 8 s (review 5958478401
// of #79, TestReviewUhidALitStoredPattern, a unit with no serial number).
// The tickle is refused at once; once the check passes, the next tickle
// reaches the unit.
func TestARefusedTickleChecksAnUncheckedLight(t *testing.T) {
	var held atomic.Bool
	light, second := failTheRecordersHandle(t, &held, false)
	second.checkTakes = 300 * time.Millisecond
	woken := make(chan struct{}, 1)
	light.checked = func() { woken <- struct{}{} }
	if r, _, _, err := light.ReadRGB(); err != nil || r != 255 { // the reopen: the unit still reads red
		t.Fatalf("control: the reopen reads %d, %v; want red", r, err)
	}

	started := time.Now()
	if err := light.ServerDown(3 * time.Second); !errors.Is(err, errLightUnchecked) {
		t.Fatalf("tickle of an unchecked unit = %v, want errLightUnchecked", err)
	}
	if took := time.Since(started); took > 100*time.Millisecond {
		t.Fatalf("the tickle of an unchecked unit took %v: it waited for the pattern check", took)
	}
	if n := second.tickleCount(); n != 0 {
		t.Fatalf("tickles that reached the unchecked unit = %d, want 0", n)
	}
	select {
	case <-woken:
	case <-time.After(3 * time.Second):
		t.Fatalf("the refused tickle started no pattern check that passed within 3s (pattern checks %d)", second.patternChecks.Load())
	}
	if err := light.ServerDown(3 * time.Second); err != nil || second.tickleCount() != 1 {
		t.Fatalf("tickle once the check passed: %v, %d reached the unit, want 1", err, second.tickleCount())
	}
}
