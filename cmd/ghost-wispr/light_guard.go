package main

import (
	"context"
	"sync"
	"time"
)

// lightGuard keeps capture closed while the recording light cannot be
// confirmed red, as the consent rule "record only while the light is on"
// requires. The light controller reports each check to report, and so does a
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
// It never blocks.
func (g *lightGuard) report(err error) {
	g.mu.Lock()
	g.fault = err
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
		g.mu.Unlock()
		g.apply(fault)
	}
}

func (g *lightGuard) apply(fault error) {
	if fault != nil {
		g.capture.HoldForLight()
		g.warnings.Set("recording-light", recordingLightWarning(fault))
		return
	}
	if !g.capture.isLightHeld() || time.Since(g.lastRelease) < g.releaseEvery {
		return
	}
	g.lastRelease = time.Now()
	g.capture.ReleaseLightHold()
}
