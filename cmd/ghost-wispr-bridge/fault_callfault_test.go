//go:build callfault

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// faultLog is a botLogs read by severity too.
type faultLog struct{ botLogs }

func (l *faultLog) logger() *slog.Logger { return newLogger(l, "bridge", slog.LevelDebug) }

// faultLine is one line of a faultLog: its severity, its message, and the
// line whole.
type faultLine struct {
	Severity, Message, Raw string
}

func (l *faultLog) lines(t *testing.T) []faultLine {
	t.Helper()
	var out []faultLine
	for raw := range strings.Lines(l.String()) {
		var rec struct{ Severity, Message string }
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", raw, err)
		}
		out = append(out, faultLine{Severity: rec.Severity, Message: rec.Message, Raw: raw})
	}
	return out
}

// waitLines waits, in real time, until at least n lines begin with
// prefix, and gives them.
func (l *faultLog) waitLines(t *testing.T, prefix string, n int) []faultLine {
	t.Helper()
	var got []faultLine
	clockWaitFor(t, func() bool {
		got = got[:0]
		for _, line := range l.lines(t) {
			if strings.HasPrefix(line.Message, prefix) {
				got = append(got, line)
			}
		}
		return len(got) >= n
	}, "%d log lines that begin with %q", n, prefix)
	return got
}

// faultNoMeetURL fails the test when a log line holds the run spec's Meet
// link: a bridge line holds only counts, ids, times and states (C9).
func faultNoMeetURL(t *testing.T, l *faultLog) {
	t.Helper()
	for _, line := range l.lines(t) {
		if strings.Contains(line.Raw, strings.TrimPrefix(faultMeetURL, "https://")) {
			t.Fatalf("a log line holds the Meet link: %s", line.Raw)
		}
	}
}

// faultRun is one run of the callfault build's applyRunSpec: the deps it
// set, its fake clock, its log, the exits it asked for, and its answer.
type faultRun struct {
	deps  bridgeDeps
	clock *fakeClock
	log   *faultLog
	exits chan int
	stop  bool
	err   error
}

// faultApply runs applyRunSpec on s and env with a fake clock at faultStart,
// and, unless env names one, an exit that the test reads.
func faultApply(t *testing.T, s *settings, env faultEnv) *faultRun {
	t.Helper()
	return faultApplyAt(t, faultStart, s, env)
}

// faultApplyAt is faultApply for a bridge that starts at start.
func faultApplyAt(t *testing.T, start time.Time, s *settings, env faultEnv) *faultRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &faultRun{clock: newFakeClock(start), log: &faultLog{}, exits: make(chan int, 1)}
	r.deps = bridgeDeps{Clock: r.clock, Log: r.log.logger()}
	if env.exit == nil {
		env.exit = func(code int) { r.exits <- code }
	}
	r.stop, r.err = applyRunSpec(ctx, s, &r.deps, env)
	return r
}

// The fault exit_bridge ends the bridge's process fault_after_s after its
// start, and not before (c-cloudrun-first reads the instance id that the
// next start logs).
func TestExitBridgeEndsTheProcessAfterFaultAfterS(t *testing.T) {
	spec := faultSpec(faultStart.Add(time.Hour), "exit_bridge")
	spec["fault_after_s"] = 600
	r := faultApply(t, &settings{RunSpecFile: faultWriteSpec(t, spec)}, faultEnv{})
	if r.stop || r.err != nil {
		t.Fatalf("applyRunSpec = stop %v, err %v; want false, nil", r.stop, r.err)
	}

	next, ok := r.clock.Next()
	if !ok || !next.Equal(faultStart.Add(600*time.Second)) {
		t.Fatalf("the clock's next waiter is at %v (%v), want the start + 600 s", next, ok)
	}
	r.clock.Advance(599 * time.Second)
	select {
	case code := <-r.exits:
		t.Fatalf("the bridge exited with %d at 599 s", code)
	default:
	}
	r.clock.Advance(time.Second)
	select {
	case code := <-r.exits:
		if code != 1 {
			t.Fatalf("the bridge exited with %d, want 1", code)
		}
	case <-time.After(clockWaitLimit):
		t.Fatal("the bridge did not exit at 600 s")
	}
	lines := r.log.waitLines(t, "FAULT exit_bridge", 1)
	if lines[0].Message != "FAULT exit_bridge after_s=600" || lines[0].Severity != "ERROR" {
		t.Fatalf("exit line = %s %q, want ERROR %q", lines[0].Severity, lines[0].Message, "FAULT exit_bridge after_s=600")
	}
}

// Each of the faults that change the bot's life sets its own flag of
// faultSet and no other; every run spec sets LeaveAt to its end time; and
// none of them starts a probe or an exit.
func TestEachBotFaultSetsOnlyItsFlag(t *testing.T) {
	end := faultStart.Add(90 * time.Minute)
	for _, tc := range []struct {
		fault string
		want  faultSet
	}{
		{"", faultSet{LeaveAt: end}},
		{"chat_check_always_passes", faultSet{ChatCheckAlwaysPasses: true, LeaveAt: end}},
		{"notice_join_only", faultSet{NoticeJoinOnly: true, LeaveAt: end}},
		{"long_lines", faultSet{LongLines: true, LeaveAt: end}},
	} {
		t.Run("fault="+tc.fault, func(t *testing.T) {
			var faults []string
			if tc.fault != "" {
				faults = []string{tc.fault}
			}
			r := faultApply(t, &settings{RunSpecFile: faultWriteSpec(t, faultSpec(end, faults...))}, faultEnv{})
			if r.stop || r.err != nil {
				t.Fatalf("applyRunSpec = stop %v, err %v; want false, nil", r.stop, r.err)
			}
			if r.deps.Faults != tc.want {
				t.Fatalf("faults = %+v, want %+v", r.deps.Faults, tc.want)
			}
			if r.deps.Link != nil {
				t.Fatalf("link = %v, want none: only fake_pi links in slice 3", r.deps.Link)
			}
			if next, ok := r.clock.Next(); ok {
				t.Fatalf("a waiter on the clock at %v: only exit_bridge and fake_pi wait", next)
			}
			lines := r.log.lines(t)
			if len(lines) != 1 || !strings.HasPrefix(lines[0].Message, "RUN-SPEC faults=") {
				t.Fatalf("log = %v, want the one RUN-SPEC line", lines)
			}
		})
	}
}

// A test bridge that starts with a run spec whose end time has passed makes
// no bot and no link, so a count set to 1 by hand replays nothing (spec, "The
// test pool"); it stays up, idle, until its context ends. It still logs
// BRIDGE-START with its instance first, which c-cloudrun-first reads. The
// control, the same start with an end time ahead, makes the run spec's bot
// through the fake Pi, and logs BRIDGE-START once: so the test sees a bot and
// a link when there is one.
func TestAnExpiredRunSpecMakesNoBotAndNoLink(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  time.Time
		bot  bool
	}{
		{"an end time ahead (control)", faultStart.Add(time.Hour), true},
		{"an end time that passed", faultStart.Add(-time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newFakeClock(faultStart)
			api := newFakeAttendee(clk)
			log := &faultLog{}
			s := settings{RunSpecFile: faultWriteSpec(t, faultSpec(tc.end, "fake_pi")), BridgeKey: []byte("bridge-key"),
				MetadataURL: botLifeMetadata(t, "faultinstance7")}
			deps := bridgeDeps{API: api, Clock: clk, Log: log.logger(), Sockets: &botSockets{}}

			b := faultStartBridge(t, &s, &deps)
			if tc.bot {
				clockWaitFor(t, func() bool { return len(api.createdSettings()) == 1 }, "the run spec's bot")
			}
			b.stillRunning(t)
			if err := b.stop(t); err != nil {
				t.Fatalf("startBridge: %v", err)
			}

			created := api.createdSettings()
			if !tc.bot {
				if len(created) != 0 || len(api.allCalls()) != 0 || deps.Link != nil {
					t.Fatalf("an expired run spec: bots %+v, Attendee calls %v, link %v; want none", created, api.allCalls(), deps.Link)
				}
				lines := log.lines(t)
				if len(lines) != 2 || lines[0].Message != "BRIDGE-START instance=faultinstance7" || lines[1].Message != "RUN-SPEC-REFUSED reason=expired" {
					t.Fatalf("log = %v, want BRIDGE-START with the instance, then RUN-SPEC-REFUSED reason=expired", lines)
				}
				return
			}
			if _, ok := deps.Link.(*fakePi); !ok {
				t.Fatalf("link = %#v, want the fake Pi", deps.Link)
			}
			if len(created) != 1 || created[0].MeetingURL != faultMeetURL || created[0].BotName != "Ghost Wispr test (recording)" {
				t.Fatalf("bots made = %+v, want one bot of the run spec's Meet link named by the fake Pi", created)
			}
			var msgs []string
			for _, l := range log.lines(t) {
				msgs = append(msgs, l.Message)
			}
			if len(msgs) == 0 || msgs[0] != "BRIDGE-START instance=faultinstance7" || botLifeStartLines(msgs) != 1 {
				t.Fatalf("log = %q, want BRIDGE-START with the instance first and once", msgs)
			}
			faultNoMeetURL(t, log)
		})
	}
}
