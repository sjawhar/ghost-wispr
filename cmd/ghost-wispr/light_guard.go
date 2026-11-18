package main

import (
	"context"
	"sync"
	"time"
)

// lightGuard keeps capture closed while the recording light cannot be
// confirmed red: a unit with a light records only while it is on. The light
// controller reports each check to report, and so does a
// red confirmation for audio that comes back after a stall
// (hardwareControl.LightUnconfirmed); a failed check holds the capture gate
// (captureGate.HoldForLight) and names the cause on /api/status, and the
// first good check after that releases the hold, so the gate opens again
// once beforeGateOpen has confirmed the light red. A person's pause is left
// as it is.
//
// The gate's lock can wait on a paused session's save, so the controller never
// takes it: report only records the result, and run applies it.
type lightGuard struct {
	capture  *captureGate
	warnings *warningsRegistry
	// releaseEvery spaces the releases, so a light that passes the
	// controller's check but fails the red confirmation is lit red at most once
	// each releaseEvery while capture is held.
	releaseEvery time.Duration

	mu      sync.Mutex
	fault   error
	checked chan struct{} // holds one signal: a check is waiting to be applied

	lastRelease time.Time // used by run alone
	pending     bool      // a check is recorded that run has not read, under mu
}

func newLightGuard(capture *captureGate, warnings *warningsRegistry, releaseEvery time.Duration) *lightGuard {
	return &lightGuard{
		capture:      capture,
		warnings:     warnings,
		releaseEvery: releaseEvery,
		checked:      make(chan struct{}, 1),
	}
}

// report records a check of the light, nil when it shows what capture wants.
// A failure that run has not read yet is kept over a later pass: the pass
// does not show the light red again, as when the controller finds the light
// off, as it wants it while nothing records, right after a failed red
// confirmation. It never blocks.
func (g *lightGuard) report(err error) {
	g.mu.Lock()
	if err != nil || !g.pending || g.fault == nil {
		g.fault = err
	}
	g.pending = true
	g.mu.Unlock()
	select {
	case g.checked <- struct{}{}:
	default:
	}
}

// run applies the latest check until ctx ends.
func (g *lightGuard) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.checked:
		}
		g.mu.Lock()
		fault := g.fault
		g.pending = false
		g.mu.Unlock()
		g.apply(fault)
	}
}

func (g *lightGuard) apply(fault error) {
	if fault != nil {
		// The warning first: once capture is held, /api/status names the light.
		g.warnings.Set("recording-light", recordingLightWarning(fault))
		g.capture.HoldForLight()
		return
	}
	// A check that passes ends a busy light's warning: another program had
	// the light, which did not fail. But not while capture stays held for the
	// light: its warning says why the unit does not record (red team, review
	// 5956273705 of #79). Any other warning waits for a red confirmation;
	// ClearIf leaves it.
	if !g.capture.isLightHeld() {
		g.warnings.ClearIf("recording-light", recordingLightBusyWarning)
		return
	}
	if time.Since(g.lastRelease) < g.releaseEvery {
		return
	}
	g.lastRelease = time.Now()
	g.capture.ReleaseLightHold()
	g.warnings.ClearIf("recording-light", recordingLightBusyWarning)
}
