package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTheLightFixturesWaitForTheirGateOpenings fails if a light fixture's
// cleanup returns while a gate opening the test started is still under way.
// An opening's light check (hardwareControl.CheckLight) reads openBlink1,
// which the cleanup restores: Sol's review of #205 found startLightRig,
// startTestLight and startMicStatesRig restoring it while an opening that
// ReleaseLightHold started still read it, a data race. Here the opening's
// light check takes 200 ms and then reads openBlink1, as CheckLight does when
// it opens the light; the subtest ends while it waits, and a cleanup
// registered before the fixture's, which runs after it, finds the check still
// under way. The test then waits for the check to end, so that under -race
// its late read meets the restore.
func TestTheLightFixturesWaitForTheirGateOpenings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(t *testing.T) *captureGate
	}{
		{"startLightRig", func(t *testing.T) *captureGate { return startLightRig(t, false).capture }},
		{"startTestLight", func(t *testing.T) *captureGate {
			capture := newCaptureGate(nil, nil, nil)
			startTestLight(t, &pluggableLight{present: true}, time.Hour, capture, &openMicFake{}, newWarningsRegistry())
			return capture
		}},
		{"startMicStatesRig", func(t *testing.T) *captureGate { return startMicStatesRig(t, statesOptions{}).capture }},
	} {
		var finished atomic.Bool
		checked := make(chan struct{})
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() {
				if !finished.Load() {
					t.Errorf("%s's cleanup returned while a gate opening was still in its light check, which reads openBlink1", tc.name)
				}
			})
			capture := tc.start(t)
			entered := make(chan struct{})
			var enter, leave sync.Once
			check := capture.checkLightPattern
			capture.checkLightPattern = func() error {
				enter.Do(func() { close(entered) })
				time.Sleep(200 * time.Millisecond)
				_ = openBlink1 // as CheckLight reads it when it opens the light
				finished.Store(true)
				leave.Do(func() { close(checked) })
				return check()
			}
			capture.HoldForLight()
			capture.ReleaseLightHold()
			select {
			case <-entered:
			case <-time.After(testWait):
				t.Fatal("control: the opening ReleaseLightHold started did not reach its light check")
			}
		})
		select {
		case <-checked:
		case <-time.After(testWait):
			t.Fatalf("%s: the opening's light check never ended", tc.name)
		}
	}
}
