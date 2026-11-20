//go:build linux && (amd64 || arm64)

package indicator

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"
)

// lockSim runs a test's handles of the blink(1) one at a time on a clock of
// its own, which it installs as lockClock. Each handle's goroutine (an actor)
// runs only while the simulation lets it, until it sleeps on the clock: in
// the gap before an exchange, in a poll for the node's lock, or in a transfer
// that a test's featureIOCTL makes take time. The simulation then wakes the
// actor whose sleep ends first, and sets the clock to that instant. So the
// order in which the handles try the node's flock is the order their waits
// give, every run, whatever the load of the machine; the flocks, and on a
// uhid blink(1) the transfers, are still the kernel's.
//
// A real sleep ends late: Go's runtime waits for a timer in whole
// milliseconds, so a sleep of 1 ms ends up to about 2 ms after it starts on an
// idle machine. So each sleep ends up to lateness after it is due, by a
// pseudo-random amount from the simulation's seed: a run is the same every
// time, and other seeds give it other orders. The kernel's flock serves its
// waiters in no order, so when two actors wake at the same instant, the one
// the product says goes first loses (loses): the recorder must go first by
// waking earlier, not by a tie.
type lockSim struct {
	t        *testing.T
	random   *rand.Rand
	lateness time.Duration
	mu       sync.Mutex
	changed  *sync.Cond
	now      time.Time
	actors   []*simActor
	running  *simActor
	failure  string
}

// simActor is one goroutine or process the simulation runs.
type simActor struct {
	name  string
	loses bool
	// wakeAt is when the actor's sleep ends; waiting while it sleeps or has
	// not started.
	wakeAt  time.Time
	waiting bool
	done    bool
	// wake lets an actor of this process run; resume does it for any actor.
	wake   chan struct{}
	resume func(now time.Time) error
	// sleeps counts the actor's sleeps by the length asked for.
	sleeps map[time.Duration]int
}

// newLockSim makes a simulation whose sleeps end up to lateness late, by
// amounts from seed, and whose clock starts at the real time, so a handle's
// last exchange before it stays in the past. It installs its clock as
// lockClock until the test ends.
func newLockSim(t *testing.T, seed uint64, lateness time.Duration) *lockSim {
	s := &lockSim{t: t, random: rand.New(rand.NewPCG(seed, seed)), lateness: lateness, now: time.Now()}
	s.changed = sync.NewCond(&s.mu)
	original := lockClock
	lockClock = s
	t.Cleanup(func() { lockClock = original })
	return s
}

// Now is the simulation's clock.
func (s *lockSim) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// Sleep lets the running actor sleep for d and the others run meanwhile.
func (s *lockSim) Sleep(d time.Duration) {
	s.mu.Lock()
	a := s.running
	if a == nil {
		s.mu.Unlock()
		panic("lockSim: a sleep on the simulation's clock from outside its actors")
	}
	s.sleep(a, d)
	s.mu.Unlock()
	<-a.wake
}

// sleep records that the running actor a sleeps for d, under s.mu.
func (s *lockSim) sleep(a *simActor, d time.Duration) {
	a.sleeps[d]++
	late := time.Duration(0)
	if s.lateness > 0 {
		late = time.Duration(s.random.Int64N(int64(s.lateness)))
	}
	a.wakeAt, a.waiting = s.now.Add(max(d, 0)+late), true
	s.running = nil
	s.changed.Broadcast()
}

// add registers an actor that starts at offset from the clock's start.
func (s *lockSim) add(name string, loses bool, offset time.Duration) *simActor {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &simActor{name: name, loses: loses, wakeAt: s.now.Add(offset), waiting: true, wake: make(chan struct{}, 1), sleeps: map[time.Duration]int{}}
	a.resume = func(time.Time) error {
		a.wake <- struct{}{}
		return nil
	}
	s.actors = append(s.actors, a)
	return a
}

// Go starts run as an actor of this process, offset after the clock's start.
func (s *lockSim) Go(name string, loses bool, offset time.Duration, run func()) *simActor {
	a := s.add(name, loses, offset)
	go func() {
		<-a.wake
		defer s.finish(a)
		run()
	}()
	return a
}

// Offset returns a pseudo-random instant in [0, within) from the seed, at
// which to start an actor.
func (s *lockSim) Offset(within time.Duration) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Duration(s.random.Int64N(int64(within)))
}

// finish records that actor a has ended.
func (s *lockSim) finish(a *simActor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.done, a.waiting = true, false
	if s.running == a {
		s.running = nil
	}
	s.changed.Broadcast()
}

// fail records a failure of the simulation itself, and ends it.
func (s *lockSim) fail(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == "" {
		s.failure = fmt.Sprintf(format, args...)
	}
	s.changed.Broadcast()
}

// Run runs the actors until every one has ended. It fails the test if an
// actor runs for stall of real time without sleeping or ending: an actor that
// waits for something the simulation does not run, such as a lock held by a
// sleeping actor that it waits for without the clock.
func (s *lockSim) Run(stall time.Duration) {
	s.t.Helper()
	tick := time.NewTicker(stall / 10)
	defer tick.Stop()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				s.mu.Lock()
				s.changed.Broadcast()
				s.mu.Unlock()
			}
		}
	}()
	for {
		s.mu.Lock()
		since := time.Now()
		for s.running != nil && s.failure == "" {
			if time.Since(since) > stall {
				s.failure = fmt.Sprintf("actor %s ran for %v without a sleep on the simulation's clock", s.running.name, stall)
				break
			}
			s.changed.Wait()
		}
		if s.failure != "" {
			failure := s.failure
			s.mu.Unlock()
			s.t.Fatal(failure)
		}
		next := s.next()
		if next == nil {
			s.mu.Unlock()
			return
		}
		s.now, next.waiting, s.running = next.wakeAt, false, next
		s.mu.Unlock()
		if err := next.resume(next.wakeAt); err != nil {
			s.t.Fatalf("wake actor %s: %v", next.name, err)
		}
	}
}

// next returns the waiting actor whose sleep ends first, nil once every actor
// has ended, under s.mu. Of the actors that wake at the same instant, those
// that lose ties come last, the rest in the order they were added.
func (s *lockSim) next() *simActor {
	waiting := slices.DeleteFunc(slices.Clone(s.actors), func(a *simActor) bool { return !a.waiting })
	if len(waiting) == 0 {
		return nil
	}
	return slices.MinFunc(waiting, func(a, b *simActor) int {
		if c := a.wakeAt.Compare(b.wakeAt); c != 0 {
			return c
		}
		switch {
		case a.loses && !b.loses:
			return 1
		case b.loses && !a.loses:
			return -1
		}
		return 0
	})
}

// Sleeps returns how many times actor a slept for d.
func (s *lockSim) Sleeps(a *simActor, d time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return a.sleeps[d]
}
