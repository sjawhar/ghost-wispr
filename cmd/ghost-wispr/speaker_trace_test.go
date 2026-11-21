package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
// speaker-trace` does not print a capture's slow cycle, longest first, and
// what held the mixer, or exits 0 on a capture with no slow cycle, which
// shows nothing. The second capture is read with a bound no cycle reaches,
// since a loaded machine can make a 5 ms turn slow.
func TestSpeakerTraceCommandPrintsTheSlowCycles(t *testing.T) {
	var out, errs bytes.Buffer
	if code := runSpeakerTraceCommand([]string{mixerTrace(t, 300*time.Millisecond)}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, errs.String())
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 2 || !strings.Contains(lines[0], "cycles of the mixer longer than 40ms") || !strings.Contains(lines[1], "a sleep inside speaker.write") {
		t.Fatalf("output does not start with the count and the 300 ms turn, a sleep inside speaker.write:\n%s", out.String())
	}
	out.Reset()
	if code := runSpeakerTraceCommand([]string{"-slow", "10s", mixerTrace(t, 0)}, &out, &errs); code != 2 || !strings.Contains(out.String(), "UNKNOWN") {
		t.Fatalf("a capture with no slow cycle: exit %d, output %q; want exit 2 and UNKNOWN", code, out.String())
	}
}
