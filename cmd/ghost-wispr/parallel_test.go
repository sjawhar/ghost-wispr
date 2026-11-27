package main

import "testing"

// parallelSlots bounds how many tests run beside each other (runParallel).
// The tests that call it mostly wait, on timers and fakes: the package used
// 29% of one CPU over its 533 s at main 3420ffa (4 CPUs, under load). The
// bound holds whatever -parallel is: on sami-devbox's 288 CPUs, Go's default
// would start them all at once, and each would wait on the others' CPU work,
// as #204 found of the concierge's tests.
var parallelSlots = make(chan struct{}, 4)

// runParallel runs t beside the other tests that call it, at most
// cap(parallelSlots) at a time, after every test that does not. A test that
// calls it, and every function of the module it reaches, neither reads nor
// writes a package-level variable that changes once the program runs (one
// assigned, whose address is taken, changed in place, or holding a lock,
// a channel, or state its methods change, such as unitProviders), nor
// changes process state (environment, working directory, default logger,
// signals): another test may change or read them meanwhile. Call it first,
// in a top-level test only: a test that holds a slot while its subtests wait
// for one would deadlock.
func runParallel(t *testing.T) {
	t.Helper()
	t.Parallel()
	parallelSlots <- struct{}{}
	t.Cleanup(func() { <-parallelSlots })
}
