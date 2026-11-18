package main

import (
	"context"
	"fmt"
	"testing"
	"time"
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
