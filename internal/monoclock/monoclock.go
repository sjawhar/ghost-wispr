// Package monoclock reads CLOCK_MONOTONIC, the clock the acceptance driver
// (deploy/pi/accept-voice.sh) times its rig recordings on. A step of the wall
// clock (settimeofday, an NTP step) does not move it, so the driver can compare
// the voice's times with its own across one. Each event the driver compares
// reads it where the event's time is taken, right beside its time.Now, and
// carries both readings: nothing converts a time.Time to this clock, so no
// conversion can be off.
package monoclock

import (
	"time"

	"golang.org/x/sys/unix"
)

// Stamp is one instant read on two clocks: Wall from time.Now, for the journal
// and for people, and Mono, CLOCK_MONOTONIC in nanoseconds, read right after
// it, the instant the driver compares; 0 when that clock could not be read.
type Stamp struct {
	Wall time.Time
	Mono int64
}

// Now reads both clocks.
func Now() Stamp {
	wall := time.Now()
	return Stamp{Wall: wall, Mono: Read()}
}

// Read returns CLOCK_MONOTONIC in nanoseconds, or 0 when it cannot be read.
func Read() int64 {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts) != nil {
		return 0
	}
	return ts.Nano()
}
