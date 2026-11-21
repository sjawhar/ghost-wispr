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
// speaker-trace` does not print a capture's slow cycle and what held the
// mixer, or exits 0 on a capture with no slow cycle, which shows nothing.
func TestSpeakerTraceCommandPrintsTheSlowCycles(t *testing.T) {
	var out, errs bytes.Buffer
	if code := runSpeakerTraceCommand([]string{mixerTrace(t, 100*time.Millisecond)}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, errs.String())
	}
	for _, want := range []string{"1 cycles of the mixer longer than 40ms", "a sleep inside speaker.write"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := runSpeakerTraceCommand([]string{mixerTrace(t, 0)}, &out, &errs); code != 2 || !strings.Contains(out.String(), "UNKNOWN") {
		t.Fatalf("a capture with no slow cycle: exit %d, output %q; want exit 2 and UNKNOWN", code, out.String())
	}
}
