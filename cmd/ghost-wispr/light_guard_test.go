package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/indicator"
)

// TestAPassingCheckDoesNotEraseAFailedConfirmation fails if the light guard
// loses a failed red confirmation because a passing check came after it and
// before run read it. The mic loop's red confirmation failed
// (LightUnconfirmed); its released red hold woke the controller, which found
// the light off, as it wants while nothing records, and reported a pass. With
// only the latest check kept, capture was not held for the light and no
// warning named it: /api/status said no_audio. CI's first run of #79 failed in
// TestFailedLightConfirmAfterAStallHoldsCaptureForTheLight on this race
// (review of #79).
func TestAPassingCheckDoesNotEraseAFailedConfirmation(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	warnings := newWarningsRegistry()
	guard := newLightGuard(capture, warnings, time.Hour)
	guard.report(fmt.Errorf("%w: it reads 0,0,0", errLightNotConfirmed))
	guard.report(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go guard.run(ctx)
	for deadline := time.Now().Add(2 * time.Second); !capture.isLightHeld(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("a failed red confirmation followed by a passing check left capture unheld: capture_state %q, warnings %v", capture.MicHold(), warnings.List())
		}
	}
	if !warningAbout(warnings, "Recording light", "does not record") {
		t.Fatalf("capture held for the light with warnings %v, want one naming the light", warnings.List())
	}

	// Control: once run has read the failure, a passing check replaces it.
	guard.report(nil)
	guard.mu.Lock()
	fault := guard.fault
	guard.mu.Unlock()
	if fault != nil {
		t.Fatalf("control: a passing check after run read the failure left the guard's check %v, want nil", fault)
	}
}

// TestABusyLightCheckLeavesNoWarningOnceItPasses fails if a light check that
// another program got in the way of (indicator.Contended: the lock was held
// past the wait) leaves a warning after the next check passes and releases
// capture's hold, or names the light as failed. On a uhid blink(1), with the
// recorder paused, a 5 s holder left "Recording light failed" on /api/status
// after the light was free again: nothing but a red confirmation cleared it
// (review 5954543056 of #79). The same holds for a unit whose stored pattern
// is not checked yet (errLightUnchecked), which the status called failed too
// (review 5958478401 of #79). A real failure keeps its warning until a red
// confirmation passes.
func TestABusyLightCheckLeavesNoWarningOnceItPasses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fault     error
		says      string
		wantAfter []string
	}{
		{"another program held the lock", fmt.Errorf("read blink(1) version: %w", indicator.ErrBusy), "busy", nil},
		{"the unit's pattern is not checked yet", errLightUnchecked, "not checked yet", nil},
		{"control: the light failed", errors.New("blink(1) feature report: input/output error"), "failed", []string{recordingLightFailedWarning}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			capture.Pause() // nothing confirms red while paused
			warnings := newWarningsRegistry()
			guard := newLightGuard(capture, warnings, time.Hour)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go guard.run(ctx)
			waitWarnings := func(what string, want func([]string) bool) []string {
				t.Helper()
				for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
					got := warnings.List()
					if want(got) {
						return got
					}
					if time.Now().After(deadline) {
						t.Fatalf("%s: warnings %v", what, got)
					}
				}
			}

			guard.report(tc.fault)
			got := waitWarnings("a failed check set no warning", func(w []string) bool { return len(w) == 1 })
			if !strings.Contains(got[0], tc.says) {
				t.Fatalf("warning for %v = %q, want it to say %q", tc.fault, got[0], tc.says)
			}
			guard.report(nil)
			time.Sleep(50 * time.Millisecond) // run applies the pass
			if got := warnings.List(); !slices.Equal(got, tc.wantAfter) {
				t.Fatalf("warnings after the next check passed = %v, want %v", got, tc.wantAfter)
			}
		})
	}
}

// TestCaptureHeldForTheLightKeepsItsWarning fails if capture stays held for
// the light with no warning on /api/status. A check that passes inside
// releaseEvery leaves the hold, and it cleared the busy warning anyway, so
// the status said the light was unavailable and not why (red team, review
// 5956273705 of #79). The busy warning now ends with the hold. Control: the
// warning of a light that failed stays, as before.
func TestCaptureHeldForTheLightKeepsItsWarning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault error
	}{
		{"control: the light failed", errors.New("blink(1) feature report: input/output error")},
		{"another program held the lock", fmt.Errorf("read blink(1) version: %w", indicator.ErrBusy)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			warnings := newWarningsRegistry()
			guard := newLightGuard(capture, warnings, time.Hour)
			guard.lastRelease = time.Now() // the last release was just now: the next waits releaseEvery
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go guard.run(ctx)

			guard.report(tc.fault)
			for deadline := time.Now().Add(2 * time.Second); !capture.isLightHeld() || len(warnings.List()) == 0; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("control: a failed check left capture held %t with warnings %v", capture.isLightHeld(), warnings.List())
				}
			}
			guard.report(nil)
			time.Sleep(50 * time.Millisecond) // run applies the pass
			if !capture.isLightHeld() {
				t.Fatal("control: a passing check inside releaseEvery released the hold")
			}
			if got := warnings.List(); len(got) == 0 {
				t.Fatalf("capture held for the light (capture_state %q) with no warning, after a check failed (%s) and the next one passed", capture.MicHold(), tc.name)
			}
		})
	}
}
