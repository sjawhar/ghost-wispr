// Package monoclock gives a time on the system's CLOCK_MONOTONIC, the clock
// the acceptance driver (deploy/pi/accept-voice.sh) times its rig recordings
// on. A step of the wall clock (settimeofday, an NTP step) moves neither, so
// the driver can compare the voice's times with its own across one.
package monoclock

import (
	"time"

	"golang.org/x/sys/unix"
)

// base and baseNs are one instant on Go's clock and on CLOCK_MONOTONIC: base
// is the middle of two reads of time.Now around the read of baseNs.
var (
	base   time.Time
	baseNs int64
	ok     bool
)

func init() {
	var ts unix.Timespec
	before := time.Now()
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts) != nil {
		return
	}
	after := time.Now()
	base, baseNs, ok = before.Add(after.Sub(before)/2), ts.Nano(), true
}

// Nanos returns t on CLOCK_MONOTONIC, in nanoseconds, and whether it could:
// t must carry Go's monotonic reading, which time.Now gives and Add and Sub
// keep. A time built from parts, parsed, or passed through UTC, Local, In or
// Round(0) has none, and only its wall clock, which a step moves, could place
// it.
func Nanos(t time.Time) (int64, bool) {
	// Round(0) strips the monotonic reading, so it changes a time that has one;
	// Equal would ignore that reading.
	if !ok || t == t.Round(0) { //nolint:staticcheck // == compares the monotonic reading.
		return 0, false
	}
	return baseNs + int64(t.Sub(base)), true
}
