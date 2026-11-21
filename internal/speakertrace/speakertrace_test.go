package speakertrace

import (
	"bytes"
	"context"
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

// fakeMixer writes frames as the speaker's mixer marks them: each write a
// speaker.write region of a 5 ms sleep, each turn a take of mu inside
// speaker.lock; its turn after write 10 also runs stall.
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
			stall()
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

// crowd keeps the mixer runnable but not run for about d: one processor for
// Go, 16 goroutines computing on it until d is up, and the mixer yielding to
// them over and over, each time to the back of the queue.
func crowd(d time.Duration) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	deadline := time.Now().Add(d)
	var spinning sync.WaitGroup
	for range 16 {
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

// TestReadNamesWhatHeldTheMixer fails if the stalled turn of the mixer is not
// found and named for what held it: the speaker's lock held by another
// goroutine, which it names, a sleep inside the write, and Go running other
// goroutines while the mixer could run; or if a turn no longer than the bound
// is returned as slow. On a loaded machine an unstalled turn can be slow too,
// so the stall's turn is the longest, not the only one.
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
		{"a sleep in the write", func(*sync.Mutex) { time.Sleep(150 * time.Millisecond) }, []string{"a sleep inside speaker.write"}},
		{"no processor free", func(*sync.Mutex) { crowd(150 * time.Millisecond) }, []string{"runnable but not run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			buf := traced(t, func() { fakeMixer(&mu, func() { tc.stall(&mu) }) })
			cycles, err := Read(buf, slow)
			if err != nil {
				t.Fatal(err)
			}
			if len(cycles) == 0 {
				t.Fatal("no slow cycle, want the one the stall made")
			}
			var c *Cycle
			for i := range cycles {
				if cycles[i].Length() <= slow {
					t.Fatalf("a cycle of %v returned as longer than %v", cycles[i].Length(), slow)
				}
				if c == nil || cycles[i].Length() > c.Length() {
					c = &cycles[i]
				}
			}
			got := c.Classify()
			if c.Length() < 140*time.Millisecond {
				t.Fatalf("the longest slow cycle took %v, want the 150 ms stall in it", c.Length())
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("the longest cycle reads %q (%v), want it to name %q", got, c.Spent, w)
				}
			}
		})
	}
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
