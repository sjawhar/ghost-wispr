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
	var out, errs bytes.Buffer
	before := time.Now()
	path := mixerTrace(t, 300*time.Millisecond)
	after := time.Now()
	if code := runSpeakerTraceCommand([]string{path}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, errs.String())
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 2 || !strings.Contains(lines[0], "cycles of the mixer longer than 40ms") || !strings.Contains(lines[1], "a sleep inside speaker.write") {
		t.Fatalf("output does not start with the count and the 300 ms turn, a sleep inside speaker.write:\n%s", out.String())
	}
	times := regexp.MustCompile(`from (\S+Z) to (\S+Z) \(\S+ into the capture\)`).FindStringSubmatch(lines[1])
	if times == nil {
		t.Fatalf("the slow turn's line gives no wall-clock times in UTC: %q", lines[1])
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
