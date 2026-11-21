package monoclock

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func clockMonotonic(t *testing.T) int64 {
	t.Helper()
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		t.Fatalf("CLOCK_MONOTONIC: %v", err)
	}
	return ts.Nano()
}

// TestNowReadsTheMonotonicClockAtTheCall fails if a Stamp's Mono is not
// CLOCK_MONOTONIC read during the call to Now: it must lie between a read of
// that clock before the call and one after, with no tolerance, as the
// acceptance driver compares it with its own reads. A conversion from Go's
// clock calibrated once at startup lands outside the bracket by as long as
// that calibration was held up.
func TestNowReadsTheMonotonicClockAtTheCall(t *testing.T) {
	for range 1000 {
		before := clockMonotonic(t)
		got := Now()
		after := clockMonotonic(t)
		if got.Mono < before || got.Mono > after {
			t.Fatalf("Now().Mono = %d, outside [%d, %d]", got.Mono, before, after)
		}
	}
}

// TestNowReadsTheWallClockBesideIt fails if a Stamp's Wall is not time.Now
// read during the same call: the journal selects lines by it.
func TestNowReadsTheWallClockBesideIt(t *testing.T) {
	before := time.Now()
	got := Now()
	after := time.Now()
	if got.Wall.Before(before) || got.Wall.After(after) {
		t.Fatalf("Now().Wall = %v, outside [%v, %v]", got.Wall, before, after)
	}
}
