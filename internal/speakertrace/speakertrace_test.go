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

// crowd keeps the mixer runnable but not run for about d: sixteen goroutines
// for each of Go's processors, computing until d is up, and the mixer
// yielding to them over and over, each time to the back of the queue. It
// leaves GOMAXPROCS alone: changing it stops the world under the trace.
func crowd(d time.Duration) {
	deadline := time.Now().Add(d)
	var spinning sync.WaitGroup
	for range 16 * runtime.GOMAXPROCS(0) {
		spinning.Go(func() {
			for time.Now().Before(deadline) {
			}
		})
	}
	for time.Now().Before(deadline) {
		runtime.Gosched()
	}
	spinning.Wait()
}

// liveHeap is a heap of a million small objects, so that each collection
// takes a while to mark. It is built once, before any stall runs: building it
// is computing, which is not what the collector control injects.
var liveHeap = sync.OnceValue(func() []*[16]byte {
	live := make([]*[16]byte, 1<<20)
	for i := range live {
		live[i] = new([16]byte)
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
		want  []string
	}{
		{"the lock held", func(mu *sync.Mutex) {
			locked := make(chan struct{})
			go holdIt(mu, 150*time.Millisecond, locked)
			<-locked // the next turn's take of the lock waits
		}, []string{"the Output's lock", "holdIt"}},
		{"the lock held, passed on by a waiter", func(mu *sync.Mutex) {
			locked := make(chan struct{})
			go holdIt(mu, 250*time.Millisecond, locked)
			<-locked
			go passIt(mu)
			time.Sleep(20 * time.Millisecond) // passIt queues for the lock before the mixer does
		}, []string{"the Output's lock", "passIt", "holdIt"}},
		{"a sleep in the write", func(*sync.Mutex) { time.Sleep(150 * time.Millisecond) }, []string{"a sleep inside " + regionFault}},
		{"no processor free", func(*sync.Mutex) { crowd(150 * time.Millisecond) }, []string{"runnable but not run", "inside " + regionFault}},
		{"garbage collection", func(*sync.Mutex) { collect(150 * time.Millisecond) }, []string{"garbage collection", "inside " + regionFault, "the collector marked for"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			liveHeap() // built before the stall, not in it
			var mu sync.Mutex
			buf := traced(t, func() { fakeMixer(&mu, func() { tc.stall(&mu) }) })
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
