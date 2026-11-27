package speakertrace

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"runtime/trace"
	"strings"
	"sync"
	"testing"
	"time"
)

// traced runs f under an execution trace and returns the trace.
func traced(t *testing.T, f func()) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := trace.Start(&buf); err != nil {
		t.Fatal(err)
	}
	f()
	trace.Stop()
	return &buf
}

// regionFault marks the injected stall, so the test finds its cycle among the
// slow ones a loaded machine makes on its own.
const regionFault = "speaker.fault"

// fakeMixer writes frames as the speaker's mixer marks them: each write a
// speaker.write region of a 5 ms sleep, each turn a take of mu inside
// speaker.lock; its turn after write 10 also runs stall, inside regionFault.
func fakeMixer(mu *sync.Mutex, stall func()) {
	ctx := context.Background()
	for i := range 20 {
		r := trace.StartRegion(ctx, RegionLock)
		mu.Lock()
		r.End()
		mu.Unlock()
		w := trace.StartRegion(ctx, RegionWrite)
		time.Sleep(5 * time.Millisecond)
		if i == 10 {
			trace.WithRegion(ctx, regionFault, stall)
		}
		w.End()
	}
}

// holdIt holds mu for d, once locked is closed.
func holdIt(mu *sync.Mutex, d time.Duration, locked chan<- struct{}) {
	mu.Lock()
	close(locked)
	time.Sleep(d)
	mu.Unlock()
}

// passIt waits for mu, yields once while it holds it, and lets it go: a
// waiter queued ahead of the mixer, through which the lock passes to it.
func passIt(mu *sync.Mutex) {
	mu.Lock()
	runtime.Gosched()
	mu.Unlock()
}

// crowd readies a crowd that keeps the mixer runnable but not run for about
// d, once the stall it returns runs: sixteen goroutines for each of Go's
// processors, computing until d is up, beside the mixer, which computes a
// millisecond at a time and yields after each. With the crowd the mixer waits
// behind it for a processor after each yield, runnable, for most of the
// stall; with no crowd it computes nearly all of it, which the reader names
// running, so its runnable time comes from the crowd. Two other mixers fail
// one way or the other: one that yields at once in a loop has a turn mostly
// runnable with no crowd at all, from its own scheduling; one that computes
// without yielding holds a processor for Go's whole 10 ms slice each time,
// and under load Linux took its thread away for long enough to make the turn
// mostly running. The goroutines start here, before the trace, and wait for
// the stall: started inside it, they were the mixer computing, 4,608 of them
// on sami-devbox's 288 processors, and at a load of 400 a turn that failed
// spent 94 of its 188 ms running. It leaves GOMAXPROCS alone: changing it
// stops the world under the trace.
func crowd(d time.Duration) (stall func()) {
	start := make(chan struct{})
	var deadline time.Time
	var spinning sync.WaitGroup
	for range 16 * runtime.GOMAXPROCS(0) {
		spinning.Go(func() {
			<-start
			for time.Now().Before(deadline) {
			}
		})
	}
	return func() {
		deadline = time.Now().Add(d)
		close(start)
		for time.Now().Before(deadline) {
			for end := time.Now().Add(time.Millisecond); time.Now().Before(end); {
			}
			runtime.Gosched()
		}
		spinning.Wait()
	}
}

// liveHeap is a heap that takes long to mark: eight arrays of a million
// pointers, each to the same small object. Each runtime.GC call runs part of
// the collection on its caller, running (it starts the collection and stops
// the world, and sweeps after the mark), and waits for the rest, the mark.
// A heap of a million small objects took about 20 ms a collection, eight or
// nine collections in the fault's 150 ms, and on a loaded machine their
// running parts once made most of the fault's turn, which the reader rightly
// named running. This heap takes 75 to 150 ms to mark, one to three
// collections in the fault, so its turn is mostly the mark the mixer waits
// for. It is built once, before any stall runs: building it is computing,
// which is not what the collector control injects.
var liveHeap = sync.OnceValue(func() [][]*[16]byte {
	target := new([16]byte)
	live := make([][]*[16]byte, 8)
	for i := range live {
		live[i] = make([]*[16]byte, 1<<20)
		for j := range live[i] {
			live[i][j] = target
		}
	}
	return live
})

// collect keeps the mixer in the garbage collector for about d, asking for
// one collection of liveHeap after another and waiting for each.
func collect(d time.Duration) {
	live := liveHeap()
	for start := time.Now(); time.Since(start) < d; {
		runtime.GC()
	}
	runtime.KeepAlive(live)
}

// TestReadNamesWhatHeldTheMixer fails if the stalled turn of the mixer is not
// found and named for what held it: the speaker's lock held by another
// goroutine, which it names, also when the lock reaches the mixer through a
// waiter queued ahead of it; a sleep inside the write; Go running other
// goroutines while the mixer could run; and the garbage collector, which the
// runtime calls "sync" as it does a mutex. It also fails if a turn no longer
// than the bound is returned as slow. On a loaded machine an unstalled turn
// can be slow too, so the stall's turn is found by its region and its cause.
func TestReadNamesWhatHeldTheMixer(t *testing.T) {
	const slow = 40 * time.Millisecond
	for _, tc := range []struct {
		name  string
		stall func(mu *sync.Mutex)
		// ready, when set, readies the stall before the trace starts, and
		// returns it.
		ready func() (stall func())
		want  []string
	}{
		{name: "the lock held", stall: func(mu *sync.Mutex) {
			locked := make(chan struct{})
			go holdIt(mu, 150*time.Millisecond, locked)
			<-locked // the next turn's take of the lock waits
		}, want: []string{"the Output's lock", "holdIt"}},
		{name: "the lock held, passed on by a waiter", stall: func(mu *sync.Mutex) {
			locked := make(chan struct{})
			go holdIt(mu, 250*time.Millisecond, locked)
			<-locked
			go passIt(mu)
			time.Sleep(20 * time.Millisecond) // passIt queues for the lock before the mixer does
		}, want: []string{"the Output's lock", "passIt", "holdIt"}},
		{name: "a sleep in the write", stall: func(*sync.Mutex) { time.Sleep(150 * time.Millisecond) }, want: []string{"a sleep inside " + regionFault}},
		{name: "no processor free", ready: func() func() { return crowd(150 * time.Millisecond) }, want: []string{"runnable but not run", "inside " + regionFault}},
		{name: "garbage collection", stall: func(*sync.Mutex) { collect(150 * time.Millisecond) }, want: []string{"garbage collection", "inside " + regionFault, "the collector marked for"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			liveHeap() // built before the stall, not in it
			stall := tc.stall
			if tc.ready != nil {
				ready := tc.ready()
				stall = func(*sync.Mutex) { ready() }
			}
			var mu sync.Mutex
			buf := traced(t, func() { fakeMixer(&mu, func() { stall(&mu) }) })
			cycles, err := Read(buf, slow)
			if err != nil {
				t.Fatal(err)
			}
			var found []string
			stalled := false
			for i := range cycles {
				c := &cycles[i]
				if c.Length() <= slow {
					t.Fatalf("a cycle of %v returned as longer than %v", c.Length(), slow)
				}
				got := c.Classify()
				found = append(found, fmt.Sprintf("%v: %s %v", c.Length().Round(time.Millisecond), got, c.Spent))
				if c.Length() >= 120*time.Millisecond && containsAll(got, tc.want) { // four fifths of the stall: a stall that ends a turn early
					stalled = true
				}
			}
			if !stalled {
				t.Fatalf("no slow cycle of at least 120ms names %q; the slow cycles: %q", tc.want, found)
			}
		})
	}
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestATraceWithNoWriteSaysSo fails if a trace taken while the speaker did
// not play reads as one with no slow cycle, a clean result, instead of an
// error.
func TestATraceWithNoWriteSaysSo(t *testing.T) {
	buf := traced(t, func() { time.Sleep(10 * time.Millisecond) })
	if _, err := Read(buf, 40*time.Millisecond); err == nil || !strings.Contains(err.Error(), "no speaker.write region") {
		t.Fatalf("Read = %v, want an error that the trace holds no speaker.write region", err)
	}
}
