// Package speakertrace reads an execution trace the speaker saved on an
// underrun (audio.SpeakerTrace, the speaker_trace_dir setting) and says where
// the speaker's mixer spent each of its slow cycles: running, waiting to run,
// in a system call, or waiting on a timer, the Output's lock or anything else,
// and inside which of the speaker's trace regions. A slow cycle is the time
// from the end of one write of a frame to the device to the end of the next,
// when it is longer than the bound given.
package speakertrace

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"golang.org/x/exp/trace"
)

// The trace regions the speaker's mixer marks (internal/audio): its write of
// one frame to the device, the pacer holding it back to real time, and its
// wait for the Output's lock.
const (
	RegionWrite = "speaker.write"
	RegionPacer = "speaker.pacer"
	RegionLock  = "speaker.lock"
)

// Cycle is one slow turn of the mixer.
type Cycle struct {
	// Start and End are the trace times, from the trace's first event, of
	// the write that ended before the cycle and the one that ended it.
	Start, End time.Duration
	// Spent is the cycle's time by what the mixer's goroutine did: its state
	// ("running", "runnable", "syscall", or "waiting (reason)", the reason as
	// the Go runtime names it), with " in REGION" while inside one of the
	// speaker's regions.
	Spent map[string]time.Duration
	// Woken names, for each wait that ended in the cycle, the goroutine that
	// ended it and the function it was in, or "the runtime" for a timer.
	Woken []string
}

// Length is how long the cycle took.
func (c *Cycle) Length() time.Duration { return c.End - c.Start }

// Dominant is the bucket of Spent that took the most time, and its time.
func (c *Cycle) Dominant() (bucket string, d time.Duration) {
	for b, t := range c.Spent {
		if t > d || t == d && b < bucket {
			bucket, d = b, t
		}
	}
	return bucket, d
}

// Classify names what kept the mixer from the device for most of the cycle.
func (c *Cycle) Classify() string {
	bucket, _ := c.Dominant()
	state, region, _ := strings.Cut(bucket, " in ")
	switch {
	case strings.HasPrefix(state, "waiting") && (region == RegionLock || strings.Contains(state, "sync")):
		return "the Output's lock: the mixer waited for its holder (" + strings.Join(c.Woken, "; ") + ")"
	case strings.HasPrefix(state, "waiting") && region == RegionPacer:
		return "the pacer's sleep"
	case strings.HasPrefix(state, "waiting") && strings.Contains(state, "sleep"):
		return "a sleep" + inRegion(region)
	case strings.HasPrefix(state, "waiting"):
		return "a wait, " + strings.TrimPrefix(state, "waiting ") + inRegion(region)
	case state == "runnable":
		return "runnable but not run: Go had no processor free for the mixer" + inRegion(region)
	case state == "running":
		return "running: computing, or its thread descheduled by Linux, which Go cannot tell apart" + inRegion(region)
	case state == "syscall":
		return "a system call or C code, such as ALSA's write" + inRegion(region)
	default:
		return bucket
	}
}

func inRegion(region string) string {
	if region == "" {
		return " outside the speaker's regions"
	}
	return " inside " + region
}

// goroutine is what the reader knows of one goroutine.
type goroutine struct {
	state   trace.GoState
	reason  string        // why it waits, while waiting
	regions []string      // the regions it is in, innermost last
	since   time.Duration // when state or regions last changed
	cycle   *Cycle        // the mixer's current cycle, once it has written
	mixer   bool          // it has begun a speaker.write region
	waited  time.Duration // when its current or last wait began
	woke    wake          // what ended its last wait
}

// wake is what ended a goroutine's wait: the goroutine that made it runnable
// (by, named name), when, the wait's start and its reason.
type wake struct {
	by           trace.GoID
	name         string
	at, waitFrom time.Duration
	reason       string
}

// chain names the goroutines that passed a lock on to the one w woke: w's
// waker, then, while that waker itself got the lock in a wait that ended
// after w's began, its waker in turn. A mutex handed through a queue of
// waiters names its long holder, not only the last waiter before the mixer.
func chain(gs map[trace.GoID]*goroutine, w wake) string {
	names := []string{w.name}
	cur := w
	for range 8 {
		g := gs[cur.by]
		if g == nil || !strings.Contains(g.woke.reason, "sync") || g.woke.at < w.waitFrom || g.woke.at > cur.at || g.woke.by == cur.by {
			break
		}
		names = append(names, g.woke.name)
		cur = g.woke
	}
	return strings.Join(names, ", woken by ")
}

func (g *goroutine) bucket() string {
	var state string
	switch g.state {
	case trace.GoRunning:
		state = "running"
	case trace.GoRunnable:
		state = "runnable"
	case trace.GoSyscall:
		state = "syscall"
	case trace.GoWaiting:
		state = "waiting (" + g.reason + ")"
	default:
		state = strings.ToLower(g.state.String())
	}
	for _, r := range slices.Backward(g.regions) {
		if strings.HasPrefix(r, "speaker.") {
			return state + " in " + r
		}
	}
	return state
}

// account adds the time since g's last change to its cycle.
func (g *goroutine) account(now time.Duration) {
	if g.cycle != nil && now > g.since {
		g.cycle.Spent[g.bucket()] += now - g.since
	}
	g.since = now
}

// Read reads an execution trace and returns the mixer's cycles longer than
// slow, in order. It fails if the trace holds no write to the device: a
// trace with none was not taken while the speaker played, and says nothing.
func Read(r io.Reader, slow time.Duration) ([]Cycle, error) {
	reader, err := trace.NewReader(r)
	if err != nil {
		return nil, err
	}
	gs := map[trace.GoID]*goroutine{}
	get := func(id trace.GoID) *goroutine {
		g := gs[id]
		if g == nil {
			g = &goroutine{}
			gs[id] = g
		}
		return g
	}
	var first trace.Time
	var cycles []Cycle
	writes := 0
	for {
		ev, err := reader.ReadEvent()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if first == 0 {
			first = ev.Time()
		}
		now := time.Duration(ev.Time() - first)
		switch ev.Kind() {
		case trace.EventStateTransition:
			st := ev.StateTransition()
			if st.Resource.Kind != trace.ResourceGoroutine {
				continue
			}
			g := get(st.Resource.Goroutine())
			g.account(now)
			from, to := st.Goroutine()
			if from == trace.GoWaiting && to == trace.GoRunnable {
				g.woke = wake{by: ev.Goroutine(), name: waker(ev), at: now, waitFrom: g.waited, reason: g.reason}
				if g.cycle != nil {
					g.cycle.Woken = append(g.cycle.Woken, chain(gs, g.woke))
				}
			}
			g.state, g.reason = to, ""
			if to == trace.GoWaiting {
				g.reason, g.waited = st.Reason, now
			}
		case trace.EventRegionBegin:
			g := get(ev.Goroutine())
			g.account(now)
			region := ev.Region().Type
			g.regions = append(g.regions, region)
			if region == RegionWrite {
				g.mixer = true
			}
		case trace.EventRegionEnd:
			g := get(ev.Goroutine())
			g.account(now)
			region := ev.Region().Type
			for i := len(g.regions) - 1; i >= 0; i-- {
				if g.regions[i] == region {
					g.regions = slices.Delete(g.regions, i, i+1)
					break
				}
			}
			if region != RegionWrite || !g.mixer {
				continue
			}
			writes++
			if c := g.cycle; c != nil {
				c.End = now
				if c.Length() > slow {
					cycles = append(cycles, *c)
				}
			}
			g.cycle = &Cycle{Start: now, Spent: map[string]time.Duration{}}
		}
	}
	if writes == 0 {
		return nil, fmt.Errorf("the trace holds no %s region: it was not taken while the speaker played", RegionWrite)
	}
	slices.SortFunc(cycles, func(a, b Cycle) int { return cmp.Compare(a.Start, b.Start) })
	return cycles, nil
}

// waker names the goroutine that made a waiting goroutine runnable, and the
// function it was in.
func waker(ev trace.Event) string {
	if ev.Goroutine() == trace.NoGoroutine {
		return "the runtime"
	}
	for frame := range ev.Stack().Frames() {
		if !strings.HasPrefix(frame.Func, "runtime.") && !strings.HasPrefix(frame.Func, "sync.") && !strings.HasPrefix(frame.Func, "internal/sync.") {
			return fmt.Sprintf("G%d in %s", ev.Goroutine(), frame.Func)
		}
	}
	return fmt.Sprintf("G%d", ev.Goroutine())
}
