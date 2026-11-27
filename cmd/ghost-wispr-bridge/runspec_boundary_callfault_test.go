//go:build callfault

package main

import (
	"testing"
	"time"
)

// A seconds count must be checked before conversion to time.Duration: an
// overflowing exit timer can end the process immediately instead of later.
func TestAnExitFaultOutsideItsRunIsRefused(t *testing.T) {
	for _, seconds := range []int{3601, int(^uint(0) >> 1)} {
		spec := faultSpec(faultStart.Add(time.Hour), "exit_bridge")
		spec["fault_after_s"] = seconds
		_, refusal := readRunSpec(faultWriteSpec(t, spec), faultStart)
		if refusal == nil || refusal.reason != "fault_after_s" {
			t.Errorf("fault_after_s=%d was not refused: %+v", seconds, refusal)
		}
	}
}

// JSON and time decoder errors can contain the input, including the Meet
// URL. Refusal logs must remain safe even for a malformed run spec.
func TestAMalformedRunSpecDoesNotLogItsMeetURL(t *testing.T) {
	spec := faultSpec(faultStart.Add(time.Hour))
	spec["end_time"] = faultMeetURL
	r := faultApply(t, &settings{RunSpecFile: faultWriteSpec(t, spec)}, faultEnv{})
	faultRefused(t, r, "malformed")
}

func TestRunSpecRefusalsDoNotLogSuppliedStrings(t *testing.T) {
	for _, field := range []string{"fault", "log_level"} {
		t.Run(field, func(t *testing.T) {
			spec := faultSpec(faultStart.Add(time.Hour))
			if field == "fault" {
				spec[field] = faultMeetURL
			} else {
				spec["worker"].(map[string]any)[field] = faultMeetURL
			}
			r := faultApply(t, &settings{RunSpecFile: faultWriteSpec(t, spec)}, faultEnv{})
			reason := field
			if field == "fault" {
				reason = "unknown_fault"
			}
			faultRefused(t, r, reason)
		})
	}
}
