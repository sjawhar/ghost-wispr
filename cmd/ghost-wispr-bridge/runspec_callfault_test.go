//go:build callfault

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// faultRefused fails the test unless r refused its run spec with reason:
// stop, no error, deps unchanged, and one RUN-SPEC-REFUSED line at ERROR.
func faultRefused(t *testing.T, r *faultRun, reason string) {
	t.Helper()
	if !r.stop || r.err != nil {
		t.Fatalf("applyRunSpec = stop %v, err %v; want true, nil", r.stop, r.err)
	}
	if r.deps.Link != nil || r.deps.Calendar != nil || r.deps.Faults != (faultSet{}) {
		t.Fatalf("a refused run spec changed the deps: link %v, calendar %v, faults %+v", r.deps.Link, r.deps.Calendar, r.deps.Faults)
	}
	lines := r.log.lines(t)
	if len(lines) != 1 || lines[0].Message != "RUN-SPEC-REFUSED reason="+reason || lines[0].Severity != "ERROR" {
		t.Fatalf("log = %v, want one ERROR line %q", lines, "RUN-SPEC-REFUSED reason="+reason)
	}
	faultNoMeetURL(t, r.log)
}

// faultAccepted fails the test unless r took its run spec.
func faultAccepted(t *testing.T, r *faultRun) {
	t.Helper()
	if r.stop || r.err != nil {
		t.Fatalf("applyRunSpec = stop %v, err %v; want false, nil; log %v", r.stop, r.err, r.log.lines(t))
	}
	if r.deps.Calendar == nil {
		t.Fatal("an accepted run spec set no calendar")
	}
}

// The test pool's worker takes a log level other than ERROR only in a run
// with fake_pi, which links to no Pi and carries no office audio (spec,
// "Logs"); the bridge refuses the rest of such a run.
func TestARunSpecWithInfoLogsAndNoFakePiIsRefused(t *testing.T) {
	end := faultStart.Add(time.Hour)
	for _, tc := range []struct {
		name     string
		logLevel string
		faults   []string
		refused  bool
	}{
		{"INFO with probe_network", "INFO", []string{"probe_network"}, true},
		{"INFO with no fault", "INFO", nil, true},
		{"DEBUG with chat_check_always_passes", "DEBUG", []string{"chat_check_always_passes"}, true},
		{"lower-case error with no fault", "error", nil, true},
		{"INFO with fake_pi", "INFO", []string{"fake_pi"}, false},
		{"INFO with fake_pi and long_lines", "INFO", []string{"long_lines", "fake_pi"}, false},
		{"ERROR with no fault", "ERROR", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := faultSpec(end, tc.faults...)
			spec["worker"].(map[string]any)["log_level"] = tc.logLevel
			r := faultApply(t, &settings{RunSpecFile: faultWriteSpec(t, spec)}, faultEnv{})
			if tc.refused {
				faultRefused(t, r, "log_level")
			} else {
				faultAccepted(t, r)
			}
		})
	}
}

// A run spec the bridge cannot use makes no bot and no link: the bridge
// refuses it whole and names why.
func TestABadRunSpecIsRefused(t *testing.T) {
	end := faultStart.Add(time.Hour)
	dir := t.TempDir()
	for _, tc := range []struct {
		name   string
		path   func(t *testing.T) string
		reason string // empty: accepted
	}{
		{"no run spec file is set", func(*testing.T) string { return "" }, "missing"},
		{"the file does not exist", func(*testing.T) string { return filepath.Join(dir, "none.json") }, "missing"},
		{"the path is a directory", func(*testing.T) string { return dir }, "unreadable"},
		{"not JSON", func(t *testing.T) string { return faultWriteFile(t, "{") }, "malformed"},
		{"two JSON objects", func(t *testing.T) string {
			return faultWriteFile(t, `{"meet_url":"`+faultMeetURL+`","end_time":"`+end.Format(time.RFC3339)+`"} {}`)
		}, "malformed"},
		{"a field the bridge does not know", func(t *testing.T) string {
			spec := faultSpec(end)
			delete(spec, "fault")
			spec["faults"] = []string{"fake_pi"}
			return faultWriteSpec(t, spec)
		}, "malformed"},
		{"a fault that is a number", func(t *testing.T) string {
			spec := faultSpec(end)
			spec["fault"] = 3
			return faultWriteSpec(t, spec)
		}, "malformed"},
		{"one fault as a name, as plan C8 writes it", func(t *testing.T) string {
			spec := faultSpec(end)
			spec["fault"] = "fake_pi"
			return faultWriteSpec(t, spec)
		}, ""},
		{"one unknown fault as a name", func(t *testing.T) string {
			spec := faultSpec(end)
			spec["fault"] = "fake-pi"
			return faultWriteSpec(t, spec)
		}, "unknown_fault"},
		{"an unknown fault", func(t *testing.T) string { return faultWriteSpec(t, faultSpec(end, "fake-pi")) }, "unknown_fault"},
		{"a fault of a later slice", func(t *testing.T) string { return faultWriteSpec(t, faultSpec(end, "ignore_chat_pause")) }, "unknown_fault"},
		{"no Meet link", func(t *testing.T) string {
			spec := faultSpec(end)
			delete(spec, "meet_url")
			return faultWriteSpec(t, spec)
		}, "meet_url"},
		{"a link outside Meet", func(t *testing.T) string {
			spec := faultSpec(end)
			spec["meet_url"] = "https://example.com/abc-defg-hij"
			return faultWriteSpec(t, spec)
		}, "meet_url"},
		{"a Meet link over plain HTTP", func(t *testing.T) string {
			spec := faultSpec(end)
			spec["meet_url"] = "http://meet.google.com/abc-defg-hij"
			return faultWriteSpec(t, spec)
		}, "meet_url"},
		{"no end time", func(t *testing.T) string {
			spec := faultSpec(end)
			delete(spec, "end_time")
			return faultWriteSpec(t, spec)
		}, "end_time"},
		{"an end time that passed", func(t *testing.T) string { return faultWriteSpec(t, faultSpec(faultStart.Add(-time.Second))) }, "expired"},
		{"an end time at the start", func(t *testing.T) string { return faultWriteSpec(t, faultSpec(faultStart)) }, "expired"},
		{"an end time 2 hours and 1 s after the start", func(t *testing.T) string {
			return faultWriteSpec(t, faultSpec(faultStart.Add(2*time.Hour+time.Second)))
		}, "too_long"},
		{"an end time 2 hours after the start", func(t *testing.T) string {
			return faultWriteSpec(t, faultSpec(faultStart.Add(2*time.Hour)))
		}, ""},
		{"exit_bridge with no fault_after_s", func(t *testing.T) string {
			spec := faultSpec(end, "exit_bridge")
			delete(spec, "fault_after_s")
			return faultWriteSpec(t, spec)
		}, "fault_after_s"},
		{"a good run spec", func(t *testing.T) string { return faultWriteSpec(t, faultSpec(end, "fake_pi")) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := faultApply(t, &settings{RunSpecFile: tc.path(t)}, faultEnv{})
			if tc.reason == "" {
				faultAccepted(t, r)
				return
			}
			faultRefused(t, r, tc.reason)
		})
	}
}

// The test bridge joins the run spec's Meet link through a calendar of one
// event, from the bridge's start to the run spec's end time, so the bridge's
// own join rules apply to it; its id stays the same when the bridge restarts.
func TestTheRunSpecIsOneEventFromTheBridgesStartToItsEndTime(t *testing.T) {
	ctx := context.Background()
	end := faultStart.Add(90 * time.Minute)
	path := faultWriteSpec(t, faultSpec(end))
	r := faultApply(t, &settings{RunSpecFile: path}, faultEnv{})
	faultAccepted(t, r)
	if r.deps.Faults.LeaveAt != end {
		t.Fatalf("LeaveAt = %v, want the end time %v", r.deps.Faults.LeaveAt, end)
	}

	evs, err := r.deps.Calendar.Events(ctx, faultStart.Add(-4*time.Hour), faultStart.Add(10*time.Minute))
	if err != nil || len(evs) != 1 {
		t.Fatalf("Events = %v, %v; want one event", evs, err)
	}
	ev := evs[0]
	if ev.ID == "" || ev.MeetURL != faultMeetURL || !ev.Start.Equal(faultStart) || !ev.End.Equal(end) || ev.Done || ev.Joins != 0 {
		t.Fatalf("event = %+v, want an id, the Meet link, start %v, end %v, not done, no join", ev, faultStart, end)
	}
	if after, err := r.deps.Calendar.Events(ctx, end, end.Add(time.Hour)); err != nil || len(after) != 0 {
		t.Fatalf("Events after the end = %v, %v; want none", after, err)
	}

	for want := 1; want <= 2; want++ {
		if n, err := r.deps.Calendar.AddJoin(ctx, ev.ID); err != nil || n != want {
			t.Fatalf("AddJoin = %d, %v; want %d", n, err, want)
		}
	}
	if err := r.deps.Calendar.MarkDone(ctx, ev.ID); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if _, err := r.deps.Calendar.AddJoin(ctx, "another-event"); err == nil {
		t.Fatal("AddJoin of another event's id: no error")
	}
	evs, _ = r.deps.Calendar.Events(ctx, faultStart, end)
	if len(evs) != 1 || !evs[0].Done || evs[0].Joins != 2 {
		t.Fatalf("after two joins and done: %+v", evs)
	}

	restartedAt := faultStart.Add(20 * time.Minute)
	restarted := faultApplyAt(t, restartedAt, &settings{RunSpecFile: path}, faultEnv{})
	again, _ := restarted.deps.Calendar.Events(ctx, restartedAt, end)
	if len(again) != 1 || again[0].ID != ev.ID || !again[0].Start.Equal(restartedAt) {
		t.Fatalf("a restarted bridge's event = %+v, want the id %q from %v", again, ev.ID, restartedAt)
	}
	faultNoMeetURL(t, r.log)
	if lines := r.log.lines(t); len(lines) != 1 || lines[0].Message != "RUN-SPEC faults= end_time="+end.Format(time.RFC3339) {
		t.Fatalf("log = %v, want one RUN-SPEC line", lines)
	}
}

// C9: RUN-SPEC-REFUSED holds the refusal's fixed reason alone, never a value
// that the run spec or its path supplied, nor an error's text.
func TestARefusedRunSpecLogsNoSuppliedText(t *testing.T) {
	const sentinel = "zebra-sentinel-7719"
	for _, tc := range []struct{ name, path, reason string }{
		{"a missing file", filepath.Join(t.TempDir(), sentinel, "spec.json"), "missing"},
		{"an unknown fault", faultWriteSpec(t, faultSpec(faultStart.Add(time.Hour), sentinel)), "unknown_fault"},
		{"a malformed object", faultWriteFile(t, `{"meet_url": "`+sentinel+`", "end_time": 7}`), "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := faultApply(t, &settings{RunSpecFile: tc.path}, faultEnv{})
			faultRefused(t, r, tc.reason)
			for _, line := range r.log.lines(t) {
				if strings.Contains(line.Raw, sentinel) {
					t.Fatalf("the refusal's line holds a supplied value: %s", line.Raw)
				}
			}
		})
	}
}
