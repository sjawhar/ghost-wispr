//go:build linux

package audio

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"runtime/trace"
	"strings"
	"testing"
	"time"
)

// TestTheMixerAndWriteAllocateNothing fails if the Output's code allocates
// while speech at 24 kHz and a chime at 48 kHz are written and mixed in steady
// state, through the ALSA stream, with a trace off and with one running. A
// goroutine that allocates during a collection's mark phase is made to help
// mark and to wait for the phase to end, so a mixer that allocates can stall
// for as long as the collector takes. It counts, by stack, what the non-test
// code of this package allocated in 100 turns of 20 ms (the memory profile at
// rate 1), so another test's goroutine cannot pass or fail it.
func TestTheMixerAndWriteAllocateNothing(t *testing.T) {
	defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
	runtime.MemProfileRate = 1
	for _, traced := range []bool{false, true} {
		out := NewOutput(OutputConfig{IdleClose: time.Hour, Open: func(string) (PlaybackStream, int, error) {
			return newALSAStream(&sinkPCM{buffer: 3840}, "sink", alsaParams{rate: 48000, channels: 1, period: 480, buffer: 3840}), 48000, nil
		}})
		ctx := context.Background()
		voice, chime := out.Begin(InputVoice, nil), out.Begin(InputChime, nil)
		speech := sine(24000, 20*time.Millisecond, 440, 8000)
		ring := sine(48000, 20*time.Millisecond, 880, 4000)
		turn := func() {
			if err := voice.Write(ctx, speech, AudioFormat{SampleRate: 24000, Channels: 1}); err != nil {
				t.Fatal(err)
			}
			if err := chime.Write(ctx, ring, mono48k); err != nil {
				t.Fatal(err)
			}
		}
		if traced {
			if err := trace.Start(io.Discard); err != nil {
				t.Fatal(err)
			}
		}
		for range 50 { // the buffers grow to their size
			turn()
		}
		before := allocationsHere()
		for range 100 {
			turn()
		}
		after := allocationsHere()
		if traced {
			trace.Stop()
		}
		_ = out.Close()
		var sites []string
		for stack, n := range after {
			if d := n - before[stack]; d > 0 {
				sites = append(sites, fmt.Sprintf("%d at %s", d, stack))
			}
		}
		if len(sites) > 0 {
			t.Errorf("with a trace running %t, the Output allocated in 100 turns of 20 ms of speech and chime: %q", traced, sites)
		}
	}
}

// allocationsHere counts, by stack, the objects allocated so far from the
// non-test code of this package (the memory profile). It leaves out the
// runtime's own cache of records for waiting goroutines (sudogs), which a
// select fills now and then.
func allocationsHere() map[string]int64 {
	runtime.GC()
	runtime.GC() // the profile can be two collections old
	n, _ := runtime.MemProfile(nil, true)
	recs := make([]runtime.MemProfileRecord, n+64)
	n, _ = runtime.MemProfile(recs, true)
	counts := map[string]int64{}
	for i := range recs[:n] {
		r := &recs[i]
		var stack []string
		ours, sudog := false, false
		frames := runtime.CallersFrames(r.Stack())
		for {
			f, more := frames.Next()
			sudog = sudog || f.Function == "runtime.acquireSudog"
			if filepath.Base(filepath.Dir(f.File)) == "audio" && !strings.HasSuffix(f.File, "_test.go") {
				ours = true
				stack = append(stack, fmt.Sprintf("%s:%d", filepath.Base(f.File), f.Line))
			}
			if !more {
				break
			}
		}
		if ours && !sudog {
			counts[strings.Join(stack, " < ")] += r.AllocObjects
		}
	}
	return counts
}

// sinkPCM is a PCM that takes every frame at once and holds nothing: the
// mixer's pacer holds it to real time.
type sinkPCM struct{ buffer int }

func (p *sinkPCM) writei(_ []int16, frames int) int { return frames }
func (p *sinkPCM) recover(rc int) int               { return rc }
func (p *sinkPCM) resume() int                      { return 0 }
func (p *sinkPCM) avail() int                       { return p.buffer }
func (p *sinkPCM) delay() (int, int)                { return 0, 0 }
func (p *sinkPCM) rewindable() int                  { return 0 }
func (p *sinkPCM) rewind(int) int                   { return 0 }
func (p *sinkPCM) status() pcmStatus                { return pcmStatus{running: true, avail: p.buffer} }
func (p *sinkPCM) dropAfterStatus() (pcmStatus, time.Duration, int) {
	return p.status(), 0, 0
}
func (p *sinkPCM) prepare() int { return 0 }
func (p *sinkPCM) close() int   { return 0 }
