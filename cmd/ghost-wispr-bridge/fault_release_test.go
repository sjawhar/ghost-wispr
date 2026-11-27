//go:build !callfault

package main

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

// faultRunSpecSymbols are functions of the callfault build's run spec path:
// the run spec's reader, its calendar of one event, the fake Pi and the
// network probe.
var faultRunSpecSymbols = []string{
	"main.readRunSpec",
	"main.applyRunSpec",
	"main.(*runSpecCalendar).Events",
	"main.(*fakePi).SendAudio",
	"main.probeNetwork",
}

// faultSymbols builds this package with tags as the bridge's image does
// (CGO_ENABLED=0) and gives the binary's symbols.
func faultSymbols(t *testing.T, tags string) map[string]bool {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ghost-wispr-bridge")
	args := []string{"build", "-o", bin}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	build := exec.Command("go", append(args, ".")...)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	out, err := exec.Command("go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatalf("go tool nm %s: %v", bin, err)
	}
	symbols := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		if f := strings.Fields(line); len(f) >= 3 {
			symbols[f[len(f)-1]] = true
		}
	}
	// A binary whose symbols were not read has not passed: it must at least
	// hold its main.
	if !symbols["main.main"] {
		t.Fatalf("go tool nm listed %d symbols of the %q build and no main.main; the check cannot see the binary", len(symbols), tags)
	}
	return symbols
}

// TestTheProductionBuildHasNoJoinByURL holds R14 for the release build: it
// joins only from the calendar. Its binary holds no function of the run
// spec path, which the callfault build's does, and a run spec file present
// changes nothing.
func TestTheProductionBuildHasNoJoinByURL(t *testing.T) {
	callfault := faultSymbols(t, "callfault")
	for _, sym := range faultRunSpecSymbols {
		if !callfault[sym] {
			t.Fatalf("the callfault build has no symbol %s, so this check cannot see the run spec path", sym)
		}
	}
	release := faultSymbols(t, "")
	for _, sym := range faultRunSpecSymbols {
		if release[sym] {
			t.Errorf("the release build holds %s of the run spec path", sym)
		}
	}

	// Through main's path, startBridge: a run spec file present, with fake_pi
	// and a Meet link of its own, changes nothing. With no event in the
	// calendar no bot is made; the control, an event in the calendar, makes
	// that event's bot, so the test sees a bot when there is one.
	const runSpecURL = "https://meet.google.com/run-spec-only"
	for _, tc := range []struct {
		name   string
		events []calendar.Event
	}{
		{"no event in the calendar", nil},
		{"an event in the calendar (control)", []calendar.Event{botCalEvent("ev1", faultStart, faultStart.Add(time.Hour))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := faultSpec(faultStart.Add(time.Hour), "fake_pi", "chat_check_always_passes", "long_lines")
			spec["meet_url"] = runSpecURL
			clk := newFakeClock(faultStart)
			api := newFakeAttendee(clk)
			cal := newFakeCalendar(clk, tc.events...)
			link := newFakeLink()
			link.recv <- *link.set(true, botPiHello(botTexts()))
			logs := &botLogs{}
			s := settings{RunSpecFile: faultWriteSpec(t, spec), BridgeKey: []byte("bridge-key")}
			deps := bridgeDeps{API: api, Link: link, Calendar: cal, Clock: clk, Log: newLogger(logs, "bridge", slog.LevelDebug), Sockets: &botSockets{}}

			b := faultStartBridge(t, &s, &deps)
			// The bridge answers the Pi's hello; a bridge that read the run spec
			// logs it, and the checks below then name what it changed.
			clockWaitFor(t, func() bool {
				return len(link.sentOf(call.TypeHello)) == 1 || strings.Contains(logs.String(), "RUN-SPEC")
			}, "the bridge's answer to the Pi's hello")
			for range 31 { // past the calendar pass every 30 s
				clk.BlockUntil(t, 1)
				clk.Advance(time.Second)
			}
			if len(tc.events) > 0 {
				clockWaitFor(t, func() bool { return len(api.createdSettings()) == 1 }, "the calendar event's bot")
			}
			b.stillRunning(t)
			if err := b.stop(t); err != nil {
				t.Fatalf("startBridge: %v", err)
			}

			created := api.createdSettings()
			if len(created) != len(tc.events) {
				t.Fatalf("bots made = %+v, want %d", created, len(tc.events))
			}
			for _, c := range created {
				if c.MeetingURL != botMeetURL {
					t.Fatalf("a bot of %q, want only the calendar's Meet link", c.MeetingURL)
				}
			}
			if deps.Link != link || deps.Calendar != cal || deps.Faults != (faultSet{}) {
				t.Fatalf("the run spec changed the bridge's deps: link %v, calendar %v, faults %+v", deps.Link, deps.Calendar, deps.Faults)
			}
			if strings.Contains(logs.String(), "RUN-SPEC") {
				t.Fatalf("the release build read its run spec: %s", logs.String())
			}
		})
	}
}
