package main

import (
	"slices"
	"testing"
)

// TestAResumeWithNoCaptureDeviceLightsNothing fails if a resume lights the
// blink(1) red while no capture device is present: the light is off while the
// microphone is missing. A resume confirmed red at the request and again
// before the gate opened, through checks that did not look for a capture
// device (ReviewPR77 on #77, round 2, finding 4). The control: with a capture
// device present, the resume confirms red.
func TestAResumeWithNoCaptureDeviceLightsNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present bool
	}{
		{name: "control: a capture device is present", present: true},
		{name: "no capture device is present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startLightRig(t, true)
			present := tc.present
			r.capture.capturePresent = func() bool { return present }
			before := len(r.device.snapshot())
			if err := r.capture.RequestResume(); err != nil {
				t.Fatalf("resume: %v", err)
			}
			waitOpen(t, r.capture)
			ops := r.device.snapshot()[before:]
			lit := slices.ContainsFunc(ops, isRedOp)
			switch {
			case tc.present && !lit:
				t.Fatalf("control: a resume with a capture device present did not confirm red: operations %v", ops)
			case !tc.present && lit:
				t.Fatalf("a resume with no capture device present lit the light red: operations since the resume %v", ops)
			}
		})
	}
}
