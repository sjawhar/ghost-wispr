// Package speakertrace reads an execution trace the speaker saved on an
// underrun (audio.SpeakerTrace, the speaker_trace_dir setting) and says where
// the speaker's mixer spent each of its slow cycles: running, waiting to run,
// in a system call, or waiting on a timer, a lock, the garbage collector or
// anything else, and inside which of the speaker's trace regions; and how much
// of the cycle the collector spent marking or with the world stopped. A slow
// cycle is the time from the end of one write of a frame to the device to the
// end of the next, when it is longer than the bound given.
package speakertrace

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/exp/trace"
)

// traceHeader is how an execution trace begins: the Go version whose format
// it is in, as runtime/trace and golang.org/x/exp/trace write and read it.
const traceHeader = "go 1.%d trace\x00\x00\x00"

// formatOf is the Go 1.x release whose trace format the header head names,
// or 0 when head is not a trace's header.
func formatOf(head []byte) int {
	var minor int
	if _, err := fmt.Sscanf(string(head), "go 1.%d trace", &minor); err != nil {
		return 0
	}
	return minor
}

// newestFormat is the newest Go 1.x trace format the reader reads: the
// highest minor version whose bare header golang.org/x/exp/trace accepts, as
// the module's copy of it decides. A Go release that changes the format
// writes traces only a newer copy reads.
var newestFormat = sync.OnceValue(func() int {
	for minor := 60; minor > 0; minor-- {
		if _, err := trace.NewReader(strings.NewReader(fmt.Sprintf(traceHeader, minor))); err == nil {
			return minor
		}
	}
	return 0
})

// openTrace opens r as an execution trace. When golang.org/x/exp/trace
// cannot read it, the error names the Go release whose format it is in and
// the newest the reader reads.
func openTrace(r io.Reader) (*trace.Reader, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(len(fmt.Sprintf(traceHeader, 99)))
	reader, err := trace.NewReader(br)
	if err == nil {
		return reader, nil
	}
	if format := formatOf(head); format > newestFormat() {
		return nil, fmt.Errorf("the trace is in Go 1.%d's format, and this reader reads formats up to Go 1.%d's: rebuild it with a newer golang.org/x/exp: %w", format, newestFormat(), err)
	}
	return nil, fmt.Errorf("read the execution trace: %w", err)
}

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
	// Start and End are the trace times of the write that ended before the
	// cycle and the one that ended it, counted from the capture's first
	// retained event: not from the process's start, so they cannot be added
	// to it.
	Start, End time.Duration
	// StartWall and EndWall are those times on the wall clock, from the
	// clock snapshots the trace keeps beside its own clock (trace.Sync's
	// ClockSnapshot): what the journal's timestamps can be matched against
	// (Wall). They are zero for a trace without a snapshot.
	StartWall, EndWall Wall
	// Spent is the cycle's time by what the mixer's goroutine did: its state
	// ("running", "runnable", "syscall", or "waiting (reason)"), with " in
	// REGION" while inside one of the speaker's regions. The reason is
	// waitCollector when the wait's stack is in the garbage collector,
	// waitMutex when it is in a sync.Mutex, and otherwise as the Go runtime
	// names it.
	Spent map[string]time.Duration
	// Woken names, for each wait that ended in the cycle, the goroutine that
	// ended it and the function it was in, or "the runtime" for a timer.
	Woken []string
	// Stopped is how much of the cycle the Go runtime held the world
	// stopped, when no goroutine runs; Marking how much of it was in the
	// collector's concurrent mark phase (the trace's ranges).
	Stopped, Marking time.Duration
}

// The reasons the reader gives a wait from its stack, in place of the Go
// runtime's: the garbage collector, whether the mixer was made to help it
// mark or waited for one of its phases, and a sync.Mutex, which the runtime
// calls "sync" as it does its own semaphores.
const (
	waitCollector = "garbage collection"
	waitMutex     = "mutex"
)

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

// Classify names what kept the mixer from the device for most of the cycle,
// and how much of it the collector stopped the world or marked.
func (c *Cycle) Classify() string {
	var what string
	bucket, _ := c.Dominant()
	state, region, _ := strings.Cut(bucket, " in ")
	switch {
	case c.Stopped*2 >= c.Length():
		what = "the Go runtime stopped the world"
	case state == "waiting ("+waitCollector+")":
		what = "garbage collection: the mixer was made to help it, or waited for its phase to end" + inRegion(region)
	case state == "waiting ("+waitMutex+")" && region == RegionLock:
		what = "the Output's lock: the mixer waited for its holder (" + strings.Join(c.Woken, "; ") + ")"
	case state == "waiting ("+waitMutex+")":
		what = "a lock: the mixer waited for its holder (" + strings.Join(c.Woken, "; ") + ")" + inRegion(region)
	case strings.HasPrefix(state, "waiting") && region == RegionPacer:
		what = "the pacer's sleep"
	case strings.HasPrefix(state, "waiting") && strings.Contains(state, "sleep"):
		what = "a sleep" + inRegion(region)
	case strings.HasPrefix(state, "waiting"):
		what = "a wait, " + strings.TrimPrefix(state, "waiting ") + inRegion(region)
	case state == "runnable":
		what = "runnable but not run: Go had no processor free for the mixer" + inRegion(region)
	case state == "running":
		what = "running: computing, or its thread descheduled by Linux, which Go cannot tell apart" + inRegion(region)
	case state == "syscall":
		what = "a system call or C code, such as ALSA's write" + inRegion(region)
	default:
		what = bucket
	}
	if c.Stopped > 0 || c.Marking > 0 {
		what += fmt.Sprintf("; the runtime stopped the world for %v, and the collector marked for %v, of the turn",
			c.Stopped.Round(100*time.Microsecond), c.Marking.Round(100*time.Microsecond))
	}
	return what
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
		if g == nil || g.woke.reason != waitMutex || g.woke.at < w.waitFrom || g.woke.at > cur.at || g.woke.by == cur.by {
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
	reader, err := openTrace(r)
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
	var stopped, marking spans
	var snapshots []snapshot
	writes := 0
	var now time.Duration
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
		now = time.Duration(ev.Time() - first)
		switch ev.Kind() {
		case trace.EventSync:
			// The reader delivers a generation's sync at its first time;
			// the snapshot may have been sampled later (snap.Trace), so
			// its own trace time is what counts.
			if snap := ev.Sync().ClockSnapshot; snap != nil {
				snapshots = append(snapshots, snapshot{at: time.Duration(snap.Trace - first), wall: snap.Wall})
			}
		case trace.EventStateTransition:
			st := ev.StateTransition()
			if st.Resource.Kind != trace.ResourceGoroutine {
				continue
			}
			g := get(st.Resource.Goroutine())
			g.account(now)
			from, to := st.Goroutine()
			if from == trace.GoWaiting && to == trace.GoRunnable {
				g.woke = wake{by: ev.Goroutine(), name: waker(&ev), at: now, waitFrom: g.waited, reason: g.reason}
				if g.cycle != nil {
					g.cycle.Woken = append(g.cycle.Woken, chain(gs, g.woke))
				}
			}
			g.state, g.reason = to, ""
			if to == trace.GoWaiting {
				g.reason, g.waited = waitReason(st.Reason, st.Stack), now
			}
		case trace.EventRangeBegin, trace.EventRangeActive, trace.EventRangeEnd:
			s := &marking
			switch name := ev.Range().Name; {
			case strings.HasPrefix(name, "stop-the-world"):
				s = &stopped
			case name != "GC concurrent mark phase":
				continue
			}
			if ev.Kind() == trace.EventRangeEnd {
				s.end(now)
			} else {
				s.begin(now)
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
	stopped.end(now)
	marking.end(now)
	slices.SortFunc(snapshots, func(a, b snapshot) int { return cmp.Compare(a.at, b.at) })
	for i := range cycles {
		c := &cycles[i]
		c.Stopped, c.Marking = stopped.overlap(c.Start, c.End), marking.overlap(c.Start, c.End)
		c.StartWall, c.EndWall = wallAt(snapshots, c.Start), wallAt(snapshots, c.End)
	}
	slices.SortFunc(cycles, func(a, b Cycle) int { return cmp.Compare(a.Start, b.Start) })
	return cycles, nil
}

// Wall is a trace time on the wall clock. The trace samples the wall clock
// beside its own clock once each generation (a snapshot), and the time is
// projected from the latest snapshot sampled before it, on the assumption
// that the wall clock did not step between them. Two cases are qualified
// (Note) instead of given as a plain time:
//   - the next snapshot shows the wall clock stepped (its offset from the
//     trace clock moved by more than stepTolerance): the step's moment is
//     unknown, so the time is only known to lie between From and To, the
//     projections from the snapshots before and after it;
//   - no snapshot was sampled before the time: it is projected back from the
//     first one after it, which is right only if the clock did not step in
//     between, and nothing in the trace can show that.
//
// From equals To when the time is one instant.
type Wall struct {
	From, To time.Time
	Note     string
}

// IsZero reports a time the trace has no snapshot for.
func (w Wall) IsZero() bool { return w.From.IsZero() }

// snapshot is one of the trace's clock snapshots: its trace time, counted
// from the capture's first event, and the wall clock then.
type snapshot struct {
	at   time.Duration
	wall time.Time
}

// project is trace time t on the wall clock, by snapshot s.
func (s snapshot) project(t time.Duration) time.Time { return s.wall.Add(t - s.at) }

// stepTolerance is how far two snapshots' offsets between the wall clock and
// the trace clock may differ and still count as one clock: NTP slews a clock
// by up to 500 ppm, so 1 ms, plus that rate over the snapshots' distance.
func stepTolerance(gap time.Duration) time.Duration {
	return time.Millisecond + gap/2000
}

// wallAt is trace time t on the wall clock, from snapshots, which are sorted
// by trace time (Wall).
func wallAt(snapshots []snapshot, t time.Duration) Wall {
	if len(snapshots) == 0 {
		return Wall{}
	}
	next := slices.IndexFunc(snapshots, func(s snapshot) bool { return s.at > t })
	if next < 0 {
		next = len(snapshots)
	}
	if next == 0 {
		after := snapshots[0]
		at := after.project(t)
		return Wall{From: at, To: at, Note: fmt.Sprintf("no clock snapshot before it: projected back from the one %v later, right only if the wall clock did not step between", (after.at - t).Round(time.Millisecond))}
	}
	before := snapshots[next-1]
	at := before.project(t)
	if next == len(snapshots) {
		return Wall{From: at, To: at}
	}
	after := snapshots[next]
	step := after.wall.Sub(before.wall) - (after.at - before.at)
	if step.Abs() <= stepTolerance(after.at-before.at) {
		return Wall{From: at, To: at}
	}
	other := after.project(t)
	from, to := at, other
	direction := "forward"
	if to.Before(from) {
		from, to, direction = to, from, "back"
	}
	return Wall{From: from, To: to, Note: fmt.Sprintf("the wall clock stepped %s by %v between the clock snapshots around it, at a moment the trace does not show: the time is only known to lie between the two shown", direction, step.Abs().Round(time.Millisecond))}
}

// spans are the stretches of one of the runtime's ranges: the world stopped,
// or the collector's concurrent mark phase. One of each runs at a time.
type spans struct {
	open bool
	from time.Duration
	done [][2]time.Duration
}

func (s *spans) begin(at time.Duration) {
	if !s.open {
		s.open, s.from = true, at
	}
}

func (s *spans) end(at time.Duration) {
	if s.open {
		s.open = false
		s.done = append(s.done, [2]time.Duration{s.from, at})
	}
}

// overlap is how much of [from, to] the spans cover.
func (s *spans) overlap(from, to time.Duration) time.Duration {
	var d time.Duration
	for _, sp := range s.done {
		if lo, hi := max(sp[0], from), min(sp[1], to); hi > lo {
			d += hi - lo
		}
	}
	return d
}

// waitReason is the reason a wait gets: waitCollector when its stack is in
// the garbage collector, waitMutex when it is in a sync.Mutex, and otherwise
// the Go runtime's own. The runtime calls both its collector's semaphores and
// a sync.Mutex "sync".
func waitReason(reason string, stack trace.Stack) string {
	for frame := range stack.Frames() {
		switch f := frame.Func; {
		case strings.HasPrefix(f, "runtime.gc") || f == "runtime.GC":
			return waitCollector
		case strings.HasPrefix(f, "sync.(*Mutex).") || strings.HasPrefix(f, "internal/sync.(*Mutex).") ||
			strings.HasPrefix(f, "sync.(*RWMutex)."):
			return waitMutex
		}
	}
	return reason
}

// waker names the goroutine that made a waiting goroutine runnable, and the
// function it was in.
func waker(ev *trace.Event) string {
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
