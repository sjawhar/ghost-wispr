package main

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeClock is a clock that moves only when a test moves it. After registers
// a waiter; Advance moves the time and fires each waiter whose time came, in
// time order. BlockUntil and WaitCalls let a test wait, in real time, for the
// code under test to reach its next wait.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []clockWaiter
	calls   int
}

type clockWaiter struct {
	at time.Time
	ch chan time.Time
}

// clockWaitLimit bounds every real-time wait of a test on the fake clock.
const clockWaitLimit = 10 * time.Second

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, clockWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the time forward by d and fires each waiter whose time is
// at or before the new time.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	sort.SliceStable(c.waiters, func(i, j int) bool { return c.waiters[i].at.Before(c.waiters[j].at) })
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// Next gives the earliest time a waiter waits for.
func (c *fakeClock) Next() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.waiters) == 0 {
		return time.Time{}, false
	}
	next := c.waiters[0].at
	for _, w := range c.waiters[1:] {
		if w.at.Before(next) {
			next = w.at
		}
	}
	return next, true
}

// Calls counts every call of After so far.
func (c *fakeClock) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// BlockUntil waits until at least n waiters wait on the clock.
func (c *fakeClock) BlockUntil(t testing.TB, n int) {
	t.Helper()
	clockWaitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.waiters) >= n
	}, "%d waiters on the fake clock", n)
}

// WaitCalls waits until After has been called at least n times in all.
func (c *fakeClock) WaitCalls(t testing.TB, n int) {
	t.Helper()
	clockWaitFor(t, func() bool { return c.Calls() >= n }, "%d calls of After", n)
}

func clockWaitFor(t testing.TB, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(clockWaitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for "+format, append([]any{clockWaitLimit}, args...)...)
		}
		time.Sleep(time.Millisecond)
	}
}

// The watcher's and the bridge's tests read each time bound off this clock,
// so a waiter must fire at its time and not before.
func TestFakeClockFiresEachWaiterAtItsTimeAndNotBefore(t *testing.T) {
	start := time.Date(2026, 12, 1, 17, 0, 0, 0, time.UTC)
	c := newFakeClock(start)
	late := c.After(30 * time.Second)
	early := c.After(10 * time.Second)
	if next, ok := c.Next(); !ok || !next.Equal(start.Add(10*time.Second)) {
		t.Fatalf("Next = %v %v, want the 10 s waiter", next, ok)
	}
	c.Advance(9 * time.Second)
	select {
	case <-early:
		t.Fatal("the 10 s waiter fired at 9 s")
	default:
	}
	c.Advance(time.Second)
	if got := <-early; !got.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("the 10 s waiter fired with %v", got)
	}
	select {
	case <-late:
		t.Fatal("the 30 s waiter fired at 10 s")
	default:
	}
	c.Advance(20 * time.Second)
	<-late
	if c.Calls() != 2 {
		t.Fatalf("Calls = %d, want 2", c.Calls())
	}
	if !c.Now().Equal(start.Add(30 * time.Second)) {
		t.Fatalf("Now = %v", c.Now())
	}
}
