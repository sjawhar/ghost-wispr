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

// TestNanosIsTheMonotonicClock fails if a time read between two reads of
// CLOCK_MONOTONIC is given outside them, so that the acceptance driver, which
// reads CLOCK_MONOTONIC itself, could not compare its times with the voice's.
func TestNanosIsTheMonotonicClock(t *testing.T) {
	for range 100 {
		before := clockMonotonic(t)
		now := time.Now()
		after := clockMonotonic(t)
		got, ok := Nanos(now)
		if !ok {
			t.Fatal("Nanos refused time.Now()")
		}
		// The base is read between two reads of the wall clock: 1 us covers it.
		if got < before-1000 || got > after+1000 {
			t.Fatalf("Nanos(time.Now()) = %d, outside [%d, %d]", got, before, after)
		}
	}
}

// TestNanosFollowsGosMonotonicReading fails if a time later by d on Go's
// monotonic reading is not later by d on CLOCK_MONOTONIC: Nanos must not read
// the wall clock, which a step moves.
func TestNanosFollowsGosMonotonicReading(t *testing.T) {
	now := time.Now()
	a, okA := Nanos(now)
	b, okB := Nanos(now.Add(1500 * time.Millisecond))
	if !okA || !okB || b-a != int64(1500*time.Millisecond) {
		t.Fatalf("Nanos(now) = %d, %v; Nanos(now + 1.5 s) = %d, %v", a, okA, b, okB)
	}
}

// TestNanosRefusesATimeWithNoMonotonicReading fails if a time with no
// monotonic reading (built from parts, parsed, or stripped by UTC or Round) is
// given a CLOCK_MONOTONIC time: only its wall clock could place it.
func TestNanosRefusesATimeWithNoMonotonicReading(t *testing.T) {
	for name, at := range map[string]time.Time{
		"built":    time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC),
		"UTC":      time.Now().UTC(),
		"Round(0)": time.Now().Round(0),
	} {
		if got, ok := Nanos(at); ok {
			t.Errorf("%s: Nanos = %d, true; want false", name, got)
		}
	}
}
