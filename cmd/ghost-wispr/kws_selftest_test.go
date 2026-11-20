package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/wake/local"
)

const kwsClips = "../../internal/wake/local/testdata"

// TestKWSSelfTestHearsOnlyTheStartPhrase fails if the release build's
// self-test passes a build whose spotter does not hear the stored start
// phrases, or hears a stored near miss, or does not time the spotter. In a
// build with no listener it must fail and say so: the release builds with the
// listener, and a release without it must not pass.
func TestKWSSelfTestHearsOnlyTheStartPhrase(t *testing.T) {
	clips, err := filepath.Glob(filepath.Join(kwsClips, "*.wav"))
	if err != nil || len(clips) < 9 {
		t.Fatalf("UNKNOWN: %d stored clips (%v), want 9", len(clips), err)
	}
	var out, errs bytes.Buffer
	code := runKWSSelfTest(clips, &out, &errs)
	if !local.Built {
		if code == 0 || !strings.Contains(errs.String(), "no paused listener") {
			t.Fatalf("a build with no listener: exit %d, %q; want a failure that says so", code, errs.String())
		}
		return
	}
	if code != 0 || strings.Count(out.String(), "PASS kws-selftest") != len(clips) || !strings.Contains(out.String(), "cpu_seconds_per_audio_second=") {
		t.Fatalf("exit %d:\n%s%s\nwant %d passes and a CPU reading", code, out.String(), errs.String(), len(clips))
	}

	// Control: a stop clip under a start clip's name must fail the test.
	dir := t.TempDir()
	stop, err := os.ReadFile(filepath.Join(kwsClips, "stop-vctk23.wav"))
	if err != nil {
		t.Fatal(err)
	}
	disguised := filepath.Join(dir, "start-but-stop.wav")
	if err := os.WriteFile(disguised, stop, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := runKWSSelfTest([]string{disguised}, &out, &errs); code == 0 {
		t.Fatalf("the stop phrase named as a start phrase passed:\n%s", out.String())
	}
}
