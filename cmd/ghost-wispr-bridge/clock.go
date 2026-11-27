package main

import "time"

// clock is the bridge's and the watcher's time: realClock in production, and
// fakeClock (clock_test.go) in tests, so each time bound is tested without
// waiting for it.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
