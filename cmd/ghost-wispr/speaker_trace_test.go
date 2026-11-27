package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime/trace"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/speakertrace"
)

// mixerTrace writes a trace of a fake mixer to a file and returns its path:
// 10 writes of 5 ms each, and, if stall is set, a sleep of stall inside the
// sixth.
func mixerTrace(t *testing.T, stall time.Duration) string {
	t.Helper()
	var buf bytes.Buffer
	if err := trace.Start(&buf); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		w := trace.StartRegion(context.Background(), speakertrace.RegionWrite)
		time.Sleep(5 * time.Millisecond)
		if i == 5 {
			time.Sleep(stall)
		}
		w.End()
	}
	trace.Stop()
	path := filepath.Join(t.TempDir(), "speaker-underrun.trace")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSpeakerTraceCommandPrintsTheSlowCycles fails if `ghost-wispr
// speaker-trace` does not print a capture's slow cycle, longest first, with
// its start and end on the wall clock, in UTC, within the time the capture was
// made, and what held the mixer; or if it exits 0 on a capture with no slow
// cycle, which shows nothing. The second capture is read with a bound no
// cycle reaches, since a loaded machine can make a 5 ms turn slow.
func TestSpeakerTraceCommandPrintsTheSlowCycles(t *testing.T) {
	runParallel(t)
	var out, errs bytes.Buffer
	before := time.Now()
	path := mixerTrace(t, 300*time.Millisecond)
	after := time.Now()
	if code := runSpeakerTraceCommand([]string{path}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, errs.String())
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 3 || !strings.Contains(lines[0], "cycles of the mixer longer than 40ms") || !strings.Contains(lines[1], "assuming the clock did not step") || !strings.Contains(lines[2], "a sleep inside speaker.write") {
		t.Fatalf("output does not start with the count, the clock's assumption and the 300 ms turn, a sleep inside speaker.write:\n%s", out.String())
	}
	times := regexp.MustCompile(`from (\S+Z) to (\S+Z) \(\S+ into the capture\)`).FindStringSubmatch(lines[2])
	if times == nil {
		t.Fatalf("the slow turn's line gives no plain wall-clock times in UTC: %q", lines[2])
	}
	for _, s := range times[1:] {
		at, err := time.Parse(wallLayout, s)
		if err != nil {
			t.Fatal(err)
		}
		if at.Before(before) || at.After(after) {
			t.Fatalf("the slow turn's time %s is outside the capture's making, %s to %s", s, before.UTC().Format(wallLayout), after.UTC().Format(wallLayout))
		}
	}
	out.Reset()
	if code := runSpeakerTraceCommand([]string{"-slow", "10s", mixerTrace(t, 0)}, &out, &errs); code != 2 || !strings.Contains(out.String(), "UNKNOWN") {
		t.Fatalf("a capture with no slow cycle: exit %d, output %q; want exit 2 and UNKNOWN", code, out.String())
	}
}

// TestSpeakerTraceCommandQualifiesTimesItCannotPin fails if the command
// prints a plain wall-clock time for a turn between clock snapshots that show
// a step, or for a turn before the first snapshot (review 6003678369 of #134:
// such times were 60 s off with no warning), or qualifies one on a steady
// clock. The traces are internal/speakertrace's clock fixtures.
func TestSpeakerTraceCommandQualifiesTimesItCannotPin(t *testing.T) {
	fixture := func(name string) string {
		return filepath.Join("..", "..", "internal", "speakertrace", "testdata", "clock", name+".trace")
	}
	interval := regexp.MustCompile(`between \S+Z and \S+Z`)
	for _, tc := range []struct {
		name     string
		interval bool
		want     []string
	}{
		{"steady", false, nil},
		{"step-forward", true, []string{"the wall clock stepped forward by 1m0s"}},
		{"step-backward", true, []string{"the wall clock stepped back by 1m0s"}},
		{"late-snapshot-step", false, []string{"no clock snapshot before it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			if code := runSpeakerTraceCommand([]string{fixture(tc.name)}, &out, &errs); code != 0 {
				t.Fatalf("exit %d; stderr %q", code, errs.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
			if got := interval.MatchString(out.String()); got != tc.interval {
				t.Errorf("an interval of times printed: %t, want %t:\n%s", got, tc.interval, out.String())
			}
			if tc.want == nil && strings.Contains(out.String(), "snapshot before it") {
				t.Errorf("a steady clock's times are qualified:\n%s", out.String())
			}
		})
	}
}
