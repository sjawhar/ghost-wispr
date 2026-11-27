package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/compute/metadata"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
)

// watchStart is the watcher's start in most watcher tests: 16:52 UTC, 8
// minutes before a 17:00 call.
var watchStart = time.Date(2026, 12, 1, 16, 52, 0, 0, time.UTC)

var watchTestSettings = watchSettings{
	OrgDomains:   []string{"example.com"},
	CalendarUser: "ghost-wispr@meet.example.org",
	Pool:         fakePoolName,
}

// watchSentinel stands for words of a meeting, or any other text an error
// holds: no watcher line, on stdout or stderr, may carry it.
const watchSentinel = "SENTINEL-meeting-words"

// watchCalendar is a calendar.Reader that gives the events a test sets, or
// its error. The watcher only reads the calendar: a write fails the test.
// During each read it runs during, when set, so a test can make a read take
// time on the fake clock.
type watchCalendar struct {
	t      *testing.T
	mu     sync.Mutex
	events []calendar.Event
	err    error
	reads  [][2]time.Time
	during func()
}

func (c *watchCalendar) Events(_ context.Context, from, to time.Time) ([]calendar.Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = append(c.reads, [2]time.Time{from, to})
	if c.during != nil {
		c.during()
	}
	if c.err != nil {
		return nil, c.err
	}
	return append([]calendar.Event(nil), c.events...), nil
}

func (c *watchCalendar) MarkDone(_ context.Context, id string) error {
	c.t.Errorf("the watcher marked %s done; it only reads the calendar", id)
	return errors.New("the watcher only reads")
}

func (c *watchCalendar) AddJoin(_ context.Context, id string) (int, error) {
	c.t.Errorf("the watcher added a join to %s; it only reads the calendar", id)
	return 0, errors.New("the watcher only reads")
}

func (c *watchCalendar) SetPending(_ context.Context, id string, _ calendar.Pending) error {
	c.t.Errorf("the watcher wrote %s's pending record; it only reads the calendar", id)
	return errors.New("the watcher only reads")
}

func (c *watchCalendar) set(events []calendar.Event, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events, c.err = events, err
}

func (c *watchCalendar) setDuring(during func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.during = during
}

func (c *watchCalendar) readList() [][2]time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][2]time.Time(nil), c.reads...)
}

// watchEvent builds a calendar event from RFC 3339 times.
func watchEvent(id, start, end string, done bool, joins int) calendar.Event {
	parse := func(s string) time.Time {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return t
	}
	return calendar.Event{ID: id, Organizer: "alice@example.com", MeetURL: "https://meet.google.com/aaa-bbbb-ccc",
		Start: parse(start), End: parse(end), Done: done, Joins: joins}
}

// watchLogBuffer holds the watcher's log lines; the watcher writes from its
// own goroutine while the test reads.
type watchLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *watchLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// watchLine is one log line's fields the tests read.
type watchLine struct {
	Severity  string            `json:"severity"`
	Message   string            `json:"message"`
	Labels    map[string]string `json:"logging.googleapis.com/labels"`
	Unlabeled string            `json:"ghost_wispr_component"` // the old top-level mark, which no line may hold
	raw       string
}

// marked reports whether the line carries component's mark as its labels
// alone (loglineMark).
func (l *watchLine) marked(component string) bool {
	return len(l.Labels) == 2 && l.Labels["ghost_wispr_marked"] == "true" && l.Labels["ghost_wispr_component"] == component && l.Unlabeled == ""
}

func (b *watchLogBuffer) lines(t *testing.T) []watchLine {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []watchLine
	for _, raw := range strings.Split(strings.TrimRight(b.buf.String(), "\n"), "\n") {
		if raw == "" {
			continue
		}
		var l watchLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("a log line is not one JSON object: %v: %s", err, raw)
		}
		l.raw = raw
		out = append(out, l)
	}
	return out
}

// messages gives the messages of the lines that begin with prefix.
func (b *watchLogBuffer) messages(t *testing.T, prefix string) []string {
	t.Helper()
	var out []string
	for _, l := range b.lines(t) {
		if strings.HasPrefix(l.Message, prefix) {
			out = append(out, l.Message)
		}
	}
	return out
}

// watchRig runs runWatch against a watchCalendar, the fake Cloud Run API and
// a fake clock, and counts its passes: each pass ends with the wait for the
// next (the clock's After).
type watchRig struct {
	t      *testing.T
	clock  *fakeClock
	cal    *watchCalendar
	pool   *fakePoolServer
	logs   *watchLogBuffer
	passes int
}

// startWatch starts the watcher at now with the pool's count at count, and
// returns once its first pass ended.
func startWatch(t *testing.T, now time.Time, cal *watchCalendar, count int64) *watchRig {
	t.Helper()
	return startWatchWith(t, now, cal, count, func(*fakePoolServer) {})
}

// startWatchWith is startWatch with the fake Cloud Run API set by setup
// before the first pass.
func startWatchWith(t *testing.T, now time.Time, cal *watchCalendar, count int64, setup func(*fakePoolServer)) *watchRig {
	t.Helper()
	cal.t = t
	pool, api := newFakePoolServer(t, count)
	pool.set(setup)
	r := &watchRig{t: t, clock: newFakeClock(now), cal: cal, pool: pool, logs: &watchLogBuffer{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	deps := watchDeps{Calendar: cal, Pool: api, Clock: r.clock, Log: newLogger(r.logs, "watcher", slog.LevelDebug)}
	go func() { done <- runWatch(ctx, watchTestSettings, deps) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runWatch: %v", err)
			}
		case <-time.After(clockWaitLimit):
			t.Errorf("runWatch did not return %v after its context ended", clockWaitLimit)
		}
	})
	r.passes = 1
	r.clock.WaitCalls(t, r.passes)
	return r
}

// pass moves the clock to the next pass and waits for it to end.
func (r *watchRig) pass() {
	r.t.Helper()
	r.clock.Advance(watchEvery)
	r.passes++
	r.clock.WaitCalls(r.t, r.passes)
}

func (r *watchRig) wantMessages(prefix string, want ...string) {
	r.t.Helper()
	if got := r.logs.messages(r.t, prefix); !slices.Equal(got, want) {
		r.t.Errorf("%s lines:\n%s\nwant:\n%s", prefix, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAFailedReadChangesNoCountAndLogsNoPass(t *testing.T) {
	t.Run("the calendar", func(t *testing.T) {
		refused := fmt.Errorf("list the bot user's events: %w", &googleapi.Error{Code: 403, Message: "Not Authorized to access this resource/api"})
		r := startWatch(t, watchStart, &watchCalendar{err: refused}, 1)
		if got := r.pool.patchList(); len(got) != 0 {
			t.Errorf("a failed read sent %d count changes: %v", len(got), got)
		}
		r.wantMessages("WATCHER-READ-FAILED", "WATCHER-READ-FAILED source=calendar status=403")
		r.wantMessages("WATCHER-PASS")

		// The control: the same rig with a good read of no event sets 0.
		r.cal.set(nil, nil)
		r.pass()
		if got := r.pool.patchList(); len(got) != 1 {
			t.Fatalf("after a good read of no event: %d count changes, want 1", len(got))
		}
		r.wantMessages("WATCHER-PASS", "WATCHER-PASS count=0 next_start=none events=")
	})
	t.Run("the pool", func(t *testing.T) {
		cal := &watchCalendar{events: []calendar.Event{watchEvent("b", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", false, 0)}}
		pool := func(f *fakePoolServer) { f.getFails = true }
		r := startWatchWith(t, watchStart, cal, 0, pool)
		if got := r.pool.patchList(); len(got) != 0 {
			t.Errorf("a failed read of the pool sent %d count changes", len(got))
		}
		r.wantMessages("WATCHER-READ-FAILED", "WATCHER-READ-FAILED source=pool status=503")
		r.wantMessages("WATCHER-PASS")
	})
}

func TestThePassLogsTheCountTheNextStartAndEachEvent(t *testing.T) {
	cal := &watchCalendar{events: []calendar.Event{
		watchEvent("early", "2026-12-01T13:00:00Z", "2026-12-01T14:00:00Z", true, 1),
		watchEvent("b", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", false, 0),
		watchEvent("c", "2026-12-01T18:00:00Z", "2026-12-01T18:45:00Z", false, 0),
	}}
	r := startWatch(t, watchStart, cal, 0)
	r.wantMessages("WATCHER-PASS", "WATCHER-PASS count=1 next_start=2026-12-01T17:00:00Z events=early:true:1,b:false:0,c:false:0")

	reads := cal.readList()
	if len(reads) != 1 {
		t.Fatalf("%d reads in the first pass, want 1", len(reads))
	}
	wantFrom, wantTo := time.Date(2026, 12, 1, 12, 52, 0, 0, time.UTC), time.Date(2026, 12, 2, 16, 52, 0, 0, time.UTC)
	if !reads[0][0].Equal(wantFrom) || !reads[0][1].Equal(wantTo) {
		t.Errorf("the pass read from %s to %s, want %s (4 hours back, for events that may hold the pool) to %s (a day ahead)",
			reads[0][0].Format(time.RFC3339), reads[0][1].Format(time.RFC3339), wantFrom.Format(time.RFC3339), wantTo.Format(time.RFC3339))
	}
	if next, ok := r.clock.Next(); !ok || !next.Equal(watchStart.Add(30*time.Second)) {
		t.Errorf("the next pass waits until %s, want %s (a pass every 30 s)", next.Format(time.RFC3339), watchStart.Add(30*time.Second).Format(time.RFC3339))
	}

	// An event under way is the next start, so a test run refuses to start.
	cal.set([]calendar.Event{
		watchEvent("early", "2026-12-01T13:00:00Z", "2026-12-01T14:00:00Z", true, 1),
		watchEvent("under-way", "2026-12-01T16:30:00Z", "2026-12-01T17:30:00Z", false, 1),
	}, nil)
	r.pass()
	cal.set(nil, nil)
	r.pass()
	r.wantMessages("WATCHER-PASS",
		"WATCHER-PASS count=1 next_start=2026-12-01T17:00:00Z events=early:true:1,b:false:0,c:false:0",
		"WATCHER-PASS count=1 next_start=2026-12-01T16:30:00Z events=early:true:1,under-way:false:1",
		"WATCHER-PASS count=0 next_start=none events=",
	)
	if got := r.pool.patchList(); len(got) != 2 {
		t.Errorf("%d count changes, want 2 (to 1, then to 0): the count is set only when it differs", len(got))
	}
}

func TestAnUnservedEventIsLogged(t *testing.T) {
	cal := &watchCalendar{events: []calendar.Event{
		watchEvent("unserved", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", false, 0),
		watchEvent("served", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", false, 1),
		watchEvent("busy", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", true, 0),
		watchEvent("later", "2026-12-01T18:00:00Z", "2026-12-01T18:30:00Z", false, 0),
	}}
	r := startWatch(t, time.Date(2026, 12, 1, 17, 29, 0, 0, time.UTC), cal, 1)
	r.pass() // 17:29:30, before the end
	r.wantMessages("EVENT-UNSERVED")
	r.pass() // 17:30:00, the end
	r.wantMessages("EVENT-UNSERVED", "EVENT-UNSERVED unserved")
	r.pass()
	r.pass()
	r.wantMessages("EVENT-UNSERVED", "EVENT-UNSERVED unserved")
}

func TestARefusedCountChangeIsLoggedAndNotPassedAsDone(t *testing.T) {
	cal := &watchCalendar{events: []calendar.Event{watchEvent("b", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", false, 0)}}
	r := startWatchWith(t, watchStart, cal, 0, func(f *fakePoolServer) { f.refuse = fakePoolActAs })
	r.wantMessages("POOL-PATCH-FAILED", "POOL-PATCH-FAILED count=1 status=403 permission=iam.serviceAccounts.actAs")
	for _, l := range r.logs.lines(t) {
		if strings.HasPrefix(l.Message, "POOL-PATCH-FAILED") && l.Severity != "ERROR" {
			t.Errorf("POOL-PATCH-FAILED at severity %s, want ERROR: %s", l.Severity, l.raw)
		}
	}
	r.wantMessages("WATCHER-PASS", "WATCHER-PASS count=0 next_start=2026-12-01T17:00:00Z events=b:false:0")

	// The control: once Cloud Run takes the change, the pass gives 1.
	r.pool.set(func(f *fakePoolServer) { f.refuse = "" })
	r.pass()
	r.wantMessages("WATCHER-PASS",
		"WATCHER-PASS count=0 next_start=2026-12-01T17:00:00Z events=b:false:0",
		"WATCHER-PASS count=1 next_start=2026-12-01T17:00:00Z events=b:false:0",
	)
}

// WATCHER-PASS gives the count Cloud Run reports, not the one the watcher
// asked for: a count change counts once its operation ends; a failed
// operation logs POOL-PATCH-FAILED with its status and permission; a change
// still pending at its bound logs POOL-PATCH-FAILED status=none; a count
// that cannot be read back logs WATCHER-READ-FAILED and no pass. It fails if
// the watcher logs the count it asked for.
func TestAPassLogsTheCountCloudRunReports(t *testing.T) {
	event := func() *watchCalendar {
		return &watchCalendar{events: []calendar.Event{watchEvent("b", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", false, 0)}}
	}
	const (
		pass0 = "WATCHER-PASS count=0 next_start=2026-12-01T17:00:00Z events=b:false:0"
		pass1 = "WATCHER-PASS count=1 next_start=2026-12-01T17:00:00Z events=b:false:0"
	)
	t.Run("an operation still pending", func(t *testing.T) {
		r := startWatchWith(t, watchStart, event(), 0, func(f *fakePoolServer) { f.hold = true })
		r.wantMessages("POOL-PATCH-FAILED", "POOL-PATCH-FAILED count=1 status=none permission=")
		r.wantMessages("WATCHER-PASS", pass0)

		// The control: once Cloud Run ends the operation, the next pass
		// reads 1 and changes nothing.
		r.pool.release()
		r.pass()
		r.wantMessages("WATCHER-PASS", pass0, pass1)
		if got := r.pool.patchList(); len(got) != 1 {
			t.Errorf("%d count changes, want 1: the second pass reads the count the operation set", len(got))
		}
	})
	t.Run("an operation that fails", func(t *testing.T) {
		r := startWatchWith(t, watchStart, event(), 0, func(f *fakePoolServer) { f.opFails = fakePoolOpActAs })
		failed := "POOL-PATCH-FAILED count=1 status=403 permission=iam.serviceAccounts.actAs"
		r.wantMessages("POOL-PATCH-FAILED", failed)
		r.wantMessages("WATCHER-PASS", pass0)

		// The control: once Cloud Run's operations end well, the next pass
		// changes the count again and gives 1.
		r.pool.set(func(f *fakePoolServer) { f.opFails = "" })
		r.pass()
		r.wantMessages("POOL-PATCH-FAILED", failed)
		r.wantMessages("WATCHER-PASS", pass0, pass1)
		if got := r.pool.patchList(); len(got) != 2 {
			t.Errorf("%d count changes, want 2", len(got))
		}
	})
	t.Run("a count that cannot be read back", func(t *testing.T) {
		r := startWatchWith(t, watchStart, event(), 0, func(f *fakePoolServer) { f.readBackFails = true })
		r.wantMessages("WATCHER-READ-FAILED", "WATCHER-READ-FAILED source=pool status=503")
		r.wantMessages("WATCHER-PASS")

		// The control: once the count reads, the next pass gives it.
		r.pool.set(func(f *fakePoolServer) { f.readBackFails = false })
		r.pass()
		r.wantMessages("WATCHER-PASS", pass1)
	})
}

// The watcher passes every 30 s on the clock, whatever a pass takes: after a
// pass that takes 20 s the next comes 10 s later, at the next 30 s mark, and
// after a pass that runs past a mark the next waits for the mark after. It
// fails if the watcher waits 30 s from the end of each pass.
func TestThePassesKeepTo30sWhenAPassTakes20s(t *testing.T) {
	cal := &watchCalendar{}
	r := startWatch(t, watchStart, cal, 0)
	for _, tc := range []struct{ at, takes, next time.Duration }{
		{30 * time.Second, 20 * time.Second, 60 * time.Second},
		{60 * time.Second, 20 * time.Second, 90 * time.Second},
		{90 * time.Second, 45 * time.Second, 150 * time.Second},
	} {
		cal.setDuring(func() { r.clock.Advance(tc.takes) })
		r.clock.Advance(watchStart.Add(tc.at).Sub(r.clock.Now()))
		r.passes++
		r.clock.WaitCalls(t, r.passes)
		if next, ok := r.clock.Next(); !ok || !next.Equal(watchStart.Add(tc.next)) {
			t.Errorf("a pass at +%v that takes %v: the next waits until +%v, want +%v",
				tc.at, tc.takes, next.Sub(watchStart), tc.next)
		}
	}
	var starts []time.Duration
	for _, read := range cal.readList() {
		starts = append(starts, read[0].Add(calendar.PoolHold).Sub(watchStart))
	}
	if want := []time.Duration{0, 30 * time.Second, 60 * time.Second, 90 * time.Second}; !slices.Equal(starts, want) {
		t.Errorf("the passes read the calendar at +%v, want +%v", starts, want)
	}
}

// watchRoundTrip answers every request a client sends with the function.
type watchRoundTrip func(*http.Request) (*http.Response, error)

func (f watchRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// No watcher line carries an error's text, on stdout or on stderr: a failed
// read, a refused count change, a failed operation and a failed start each
// log a bounded class, and the sentinel inside each error stays out. It
// fails if a line copies an error's text, such as a refusal's message or a
// permission slot that holds other words.
func TestTheWatcherLogsNoErrorText(t *testing.T) {
	t.Run("a pass", func(t *testing.T) {
		refusal := `{"error":{"code":403,"message":"Permission '` + watchSentinel + `' denied on resource: ` + watchSentinel + `",` +
			`"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"permission":"` + watchSentinel + `"}}]}}`
		cal := &watchCalendar{err: fmt.Errorf("list the bot user's events: %w", &googleapi.Error{Code: 500, Message: watchSentinel})}
		r := startWatchWith(t, watchStart, cal, 0, func(f *fakePoolServer) { f.refuse = refusal })
		cal.set([]calendar.Event{watchEvent("b", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", false, 0)}, nil)
		r.pass()
		r.pool.set(func(f *fakePoolServer) { f.refuse, f.opFails = "", `{"code":7,"message":"`+watchSentinel+`"}` })
		r.pass()
		r.wantMessages("WATCHER-READ-FAILED", "WATCHER-READ-FAILED source=calendar status=500")
		r.wantMessages("POOL-PATCH-FAILED", "POOL-PATCH-FAILED count=1 status=403 permission=", "POOL-PATCH-FAILED count=1 status=403 permission=")
		r.logs.mu.Lock()
		defer r.logs.mu.Unlock()
		if strings.Contains(r.logs.buf.String(), watchSentinel) {
			t.Errorf("an error's text reached the watcher's lines:\n%s", r.logs.buf.String())
		}
	})
	t.Run("a failed start", func(t *testing.T) {
		t.Setenv("GCE_METADATA_HOST", "")
		refuse := watchRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(watchSentinel)), Request: r}, nil
		})
		env := watchTestEnv()
		stdout := &watchLogBuffer{}
		var stderr bytes.Buffer
		code := watchModeWith(context.Background(), func(k string) string { return env[k] }, stdout, &stderr,
			&googleClients{metadata: metadata.NewClient(&http.Client{Transport: refuse})})
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		var failed []map[string]any
		for _, l := range stdout.lines(t) {
			if l.Message == "WATCHER-FAILED" {
				var fields map[string]any
				if err := json.Unmarshal([]byte(l.raw), &fields); err != nil {
					t.Fatalf("WATCHER-FAILED line: %v: %s", err, l.raw)
				}
				failed = append(failed, fields)
			}
		}
		if len(failed) != 1 || failed[0]["step"] != "calendar" || failed[0]["error"] != "status=none reason=other" {
			t.Errorf("WATCHER-FAILED lines %v, want one with step calendar and error \"status=none reason=other\"", failed)
		}
		if all := stdout.buf.String() + stderr.String(); strings.Contains(all, watchSentinel) {
			t.Errorf("an error's text reached the output: stdout %q stderr %q", stdout.buf.String(), stderr.String())
		}
	})
}

func TestOnlyWatcherPassIsNotice(t *testing.T) {
	cal := &watchCalendar{events: []calendar.Event{
		watchEvent("unserved", "2026-12-01T16:00:00Z", "2026-12-01T16:30:00Z", false, 0),
		watchEvent("b", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", false, 0),
	}}
	r := startWatchWith(t, watchStart, cal, 0, func(f *fakePoolServer) { f.refuse = fakePoolActAs })
	cal.set(nil, errors.New("context deadline exceeded"))
	r.pass()
	cal.set(nil, nil)
	r.pool.set(func(f *fakePoolServer) { f.getFails = true })
	r.pass()

	seen := map[string]int{}
	for _, l := range r.logs.lines(t) {
		kind, _, _ := strings.Cut(l.Message, " ")
		seen[kind]++
		if !l.marked("watcher") {
			t.Errorf("a line without the watcher's mark as its labels: %s", l.raw)
		}
		if (kind == "WATCHER-PASS") != (l.Severity == "NOTICE") {
			t.Errorf("%s at severity %s: only WATCHER-PASS is NOTICE: %s", kind, l.Severity, l.raw)
		}
	}
	// A check that saw none of a kind has not checked it.
	for _, kind := range []string{"WATCHER-START", "WATCHER-PASS", "WATCHER-READ-FAILED", "POOL-PATCH-FAILED", "EVENT-UNSERVED"} {
		if seen[kind] == 0 {
			t.Errorf("no %s line was written, so its severity was not checked; lines: %v", kind, seen)
		}
	}
	if seen["WATCHER-READ-FAILED"] != 2 {
		t.Errorf("%d WATCHER-READ-FAILED lines, want 2 (the calendar's, then the pool's)", seen["WATCHER-READ-FAILED"])
	}
}

func TestTheWatcherSendsZeroAsACount(t *testing.T) {
	r := startWatch(t, watchStart, &watchCalendar{}, 1)
	patches := r.pool.patchList()
	if len(patches) != 1 {
		t.Fatalf("%d count changes, want 1 (from 1 to 0)", len(patches))
	}
	p := patches[0]
	if got := p.query.Get("updateMask"); got != "scaling.manualInstanceCount" {
		t.Errorf("update mask %q, want scaling.manualInstanceCount", got)
	}
	if !strings.Contains(p.body, `"manualInstanceCount":0`) {
		t.Errorf("the patch's body %s does not send \"manualInstanceCount\":0", p.body)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(p.body), &body); err != nil || len(body) != 1 || body["scaling"] == nil {
		t.Errorf("the patch's body %s, want only scaling (err %v)", p.body, err)
	}
	r.wantMessages("WATCHER-PASS", "WATCHER-PASS count=0 next_start=none events=")
	var count int64
	r.pool.set(func(f *fakePoolServer) { count = f.count })
	if count != 0 {
		t.Errorf("the pool's count is %d after the pass, want 0", count)
	}
}

// watchTestEnv is the watch mode's settings with neutral values.
func watchTestEnv() map[string]string {
	return map[string]string{
		"BRIDGE_ORG_DOMAINS":   "example.com, example.org",
		"BRIDGE_TESTERS":       "alice@example.com",
		"BRIDGE_CALENDAR_USER": "ghost-wispr@meet.example.org",
		"BRIDGE_POOL":          fakePoolName,
	}
}

func TestTheWatchSettingsAreReadFromTheEnvironment(t *testing.T) {
	getenv := func(env map[string]string) func(string) string { return func(k string) string { return env[k] } }
	got, err := watchSettingsFromEnv(getenv(watchTestEnv()))
	if err != nil {
		t.Fatalf("watchSettingsFromEnv: %v", err)
	}
	want := watchSettings{
		OrgDomains:   []string{"example.com", "example.org"},
		Testers:      []string{"alice@example.com"},
		CalendarUser: "ghost-wispr@meet.example.org",
		Pool:         fakePoolName,
	}
	if !slices.Equal(got.OrgDomains, want.OrgDomains) || !slices.Equal(got.Testers, want.Testers) ||
		got.CalendarUser != want.CalendarUser || got.Pool != want.Pool {
		t.Errorf("settings %+v, want %+v", got, want)
	}

	env := watchTestEnv()
	delete(env, "BRIDGE_TESTERS")
	if got, err := watchSettingsFromEnv(getenv(env)); err != nil || len(got.Testers) != 0 {
		t.Errorf("no tester list: testers %q, err %v; want none and no error", got.Testers, err)
	}

	for _, tc := range []struct{ key, value string }{
		{"BRIDGE_ORG_DOMAINS", ""},
		{"BRIDGE_ORG_DOMAINS", " , "},
		{"BRIDGE_CALENDAR_USER", ""},
		{"BRIDGE_POOL", ""},
		{"BRIDGE_POOL", "call-pool"},
		{"BRIDGE_POOL", "projects/example-project/locations/us-central1/services/call-pool"},
	} {
		env := watchTestEnv()
		env[tc.key] = tc.value
		if _, err := watchSettingsFromEnv(getenv(env)); err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%q: err %v, want one that names %s", tc.key, tc.value, err, tc.key)
		}
	}
}

// The command's watch mode reads the watch settings: a refused one exits 2
// with one marked line on stdout, which names each refused setting and why
// but never its value, and nothing on stderr; an unknown mode still prints
// the usage on stderr.
func TestTheWatchModeRefusesAMissingSetting(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(env map[string]string)
		want string
	}{
		{"a missing setting", func(env map[string]string) { delete(env, "BRIDGE_POOL") }, "SETTINGS-REFUSED BRIDGE_POOL=unset"},
		{"a malformed setting and a blank one", func(env map[string]string) {
			env["BRIDGE_POOL"] = "projects/" + watchSentinel
			env["BRIDGE_CALENDAR_USER"] = " "
		}, "SETTINGS-REFUSED BRIDGE_CALENDAR_USER=unset BRIDGE_POOL=malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := watchTestEnv()
			tc.edit(env)
			stdout := &watchLogBuffer{}
			var stderr bytes.Buffer
			code := run(context.Background(), []string{"watch"}, func(k string) string { return env[k] }, stdout, &stderr)
			if code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr %q, want nothing: the watch mode writes only marked lines, on stdout", stderr.String())
			}
			lines := stdout.lines(t)
			if len(lines) != 1 || lines[0].Message != tc.want || lines[0].Severity != "ERROR" || !lines[0].marked("watcher") {
				t.Errorf("stdout lines %+v, want one ERROR line of the watcher: %q", lines, tc.want)
			}
			if strings.Contains(stdout.buf.String()+stderr.String(), watchSentinel) {
				t.Errorf("a refused setting's value reached the output: stdout %q stderr %q", stdout.buf.String(), stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"wach"}, func(string) string { return "" }, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("an unknown mode: exit %d, stderr %q; want exit 2 and the usage", code, stderr.String())
	}
}

// watchModeEvent is a call's event as the Calendar API answers it: organized
// by organizer, inviting the bot user, with a Google Meet conference.
func watchModeEvent(id, organizer string, start time.Time) map[string]any {
	return map[string]any{
		"id":        id,
		"status":    "confirmed",
		"organizer": map[string]any{"email": organizer},
		"start":     map[string]any{"dateTime": start.Format(time.RFC3339)},
		"end":       map[string]any{"dateTime": start.Add(30 * time.Minute).Format(time.RFC3339)},
		"attendees": []map[string]any{{"email": organizer, "organizer": true}, {"email": "ghost-wispr@meet.example.org"}},
		"conferenceData": map[string]any{
			"conferenceSolution": map[string]any{"key": map[string]any{"type": "hangoutsMeet"}},
			"entryPoints":        []map[string]any{{"entryPointType": "video", "uri": "https://meet.google.com/aaa-bbbb-ccc"}},
		},
	}
}

// The watch mode, from its settings to its first pass, against a fake
// metadata server, signJwt, token endpoint, Calendar API and Cloud Run API.
func TestTheWatchModeReadsTheCalendarReadOnlyAsTheBotUser(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "")
	google := &dwdGoogle{}
	start := time.Now().UTC().Truncate(time.Second).Add(5 * time.Minute)
	var calMu sync.Mutex
	var authorization []string
	cal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calMu.Lock()
		authorization = append(authorization, r.Header.Get("Authorization"))
		calMu.Unlock()
		if r.URL.Path != "/calendar/v3/calendars/primary/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "calendar#events", "items": []any{
			watchModeEvent("tester-call", "alice@example.com", start),
			watchModeEvent("other-call", "carol@example.com", start.Add(time.Hour)),
		}})
	}))
	t.Cleanup(cal.Close)
	pool, poolOpts := startFakePoolServer(t, 0)

	env := watchTestEnv()
	stdout := &watchLogBuffer{}
	var stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := make(chan int, 1)
	go func() {
		code <- watchModeWith(ctx, func(k string) string { return env[k] }, stdout, &stderr, &googleClients{
			metadata: metadata.NewClient(&http.Client{Transport: google}),
			signIn:   []option.ClientOption{option.WithHTTPClient(&http.Client{Transport: google})},
			calendar: []option.ClientOption{option.WithEndpoint(cal.URL + "/calendar/v3/")},
			run:      poolOpts,
		})
	}()
	clockWaitFor(t, func() bool {
		select {
		case c := <-code:
			code <- c
			return true
		default:
			return len(stdout.messages(t, "WATCHER-PASS")) > 0
		}
	}, "the watch mode's first WATCHER-PASS")
	cancel()
	select {
	case c := <-code:
		if c != 0 {
			t.Errorf("the watch mode exited %d, want 0 after its context ended; stderr: %s", c, stderr.String())
		}
	case <-time.After(clockWaitLimit):
		t.Fatal("the watch mode did not return after its context ended")
	}

	if got, want := stdout.messages(t, "WATCHER-PASS"), fmt.Sprintf("WATCHER-PASS count=1 next_start=%s events=tester-call:false:0", start.Format(time.RFC3339)); len(got) != 1 || got[0] != want {
		t.Errorf("WATCHER-PASS lines %q, want [%q]: only the tester's event counts", got, want)
	}
	if got := stdout.messages(t, "WATCHER-START"); len(got) != 1 || got[0] != "WATCHER-START pool="+fakePoolName+" org_domains=2 testers=1" {
		t.Errorf("WATCHER-START lines %q", got)
	}
	google.mu.Lock()
	claims := google.claims
	google.mu.Unlock()
	if len(claims) != 1 {
		t.Fatalf("%d signed JWTs, want 1", len(claims))
	}
	if claims[0]["scope"] != "https://www.googleapis.com/auth/calendar.readonly" || claims[0]["sub"] != "ghost-wispr@meet.example.org" {
		t.Errorf("the watcher signed in for scope %v as %v, want calendar.readonly as the bot user", claims[0]["scope"], claims[0]["sub"])
	}
	calMu.Lock()
	defer calMu.Unlock()
	if len(authorization) == 0 || authorization[0] != "Bearer delegated-token" {
		t.Errorf("the calendar was read with Authorization %q, want the delegated token", authorization)
	}
	if patches := pool.patchList(); len(patches) != 1 || !strings.Contains(patches[0].body, `"manualInstanceCount":1`) {
		t.Errorf("count changes %v, want one to 1 on the pool BRIDGE_POOL names", patches)
	}
}
