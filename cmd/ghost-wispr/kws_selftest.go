package main

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/wake/local"
)

// kwsSelfTestSeconds is how much audio the self-test times the spotter on.
const kwsSelfTestSeconds = 60

// runKWSSelfTest is `ghost-wispr kws-selftest CLIP...`, the release build's
// check, on the architecture it is built for, that the paused listener's
// spotter links, loads its embedded model and hears the start phrase. Each
// clip, a stored 16 kHz WAV, plays between seconds of silence: one whose name
// starts with "start-" or "ghost-start-" must be heard, and every other, a
// near miss, must not. It then times
// both stages on kwsSelfTestSeconds of those clips and prints the CPU they
// take per second of audio. It never opens a microphone.
func runKWSSelfTest(args []string, stdout, stderr io.Writer) int {
	if !local.Built {
		_, _ = fmt.Fprintln(stderr, "kws-selftest: this build has no paused listener (built without -tags kws)")
		return 1
	}
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: ghost-wispr kws-selftest CLIP.wav...")
		return 2
	}
	d, err := local.NewDetector()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kws-selftest: %v\n", err)
		return 1
	}
	defer d.Close()
	silence := make([]int16, local.SampleRate)
	var timing []int16
	failed := false
	for _, clip := range args {
		pcm, err := local.ReadWAV(clip)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "kws-selftest: %v\n", err)
			return 1
		}
		audio := slices.Concat(silence, pcm, silence)
		timing = append(timing, audio...)
		name := filepath.Base(clip)
		want := strings.HasPrefix(name, "start-") || strings.HasPrefix(name, "ghost-start-")
		d.Reset()
		got := feedSelfTest(d, audio)
		status := "PASS"
		if got != want {
			status, failed = "FAIL", true
		}
		_, _ = fmt.Fprintf(stdout, "%s kws-selftest %s: heard %t, want %t\n", status, filepath.Base(clip), got, want)
	}
	var samples int
	started := processCPU()
	for samples < kwsSelfTestSeconds*local.SampleRate {
		d.Reset()
		feedSelfTest(d, timing)
		samples += len(timing)
	}
	used := processCPU() - started
	_, _ = fmt.Fprintf(stdout, "kws-selftest: cpu_seconds_per_audio_second=%.4f over %d s of audio, one thread\n", used.Seconds()/(float64(samples)/local.SampleRate), samples/local.SampleRate)
	if failed {
		return 1
	}
	return 0
}

// feedSelfTest gives audio to d in 100 ms buffers and reports whether d heard
// the phrase in it.
func feedSelfTest(d *local.Detector, audio []int16) bool {
	heard := false
	for at := 0; at < len(audio); at += local.SampleRate / 10 {
		heard = d.Write(audio[at:min(at+local.SampleRate/10, len(audio))]) || heard
	}
	return heard
}

// processCPU is the process's user and system CPU time.
func processCPU() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
