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
// phrases, or hears the stored stop phrase, or does not time the spotter. In
// a build with no listener it must fail and say so: the release builds with
// the listener, and a release without it must not pass.
func TestKWSSelfTestHearsOnlyTheStartPhrase(t *testing.T) {
	clips := []string{filepath.Join(kwsClips, "start.wav"), filepath.Join(kwsClips, "ghost-start.wav"), filepath.Join(kwsClips, "stop.wav")}
	var out, errs bytes.Buffer
	code := runKWSSelfTest(clips, &out, &errs)
	if !local.Built {
		if code == 0 || !strings.Contains(errs.String(), "no paused listener") {
			t.Fatalf("a build with no listener: exit %d, %q; want a failure that says so", code, errs.String())
		}
		return
	}
	if code != 0 || strings.Count(out.String(), "PASS kws-selftest") != 3 || !strings.Contains(out.String(), "cpu_seconds_per_audio_second=") {
		t.Fatalf("exit %d:\n%s%s\nwant three passes and a CPU reading", code, out.String(), errs.String())
	}

	// Control: the stop clip under a start clip's name must fail the test.
	dir := t.TempDir()
	stop, err := os.ReadFile(filepath.Join(kwsClips, "stop.wav"))
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
