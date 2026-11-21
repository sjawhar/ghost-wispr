//go:build linux

package audio

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/trace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/speakertrace"
)

// injectStream is the speaker's ALSA stream on a fake device, with a fault
// injected into the mixer's path at its at'th write.
type injectStream struct {
	*alsaStream
	writes atomic.Int64
	at     int64
	fault  func()
}

func (s *injectStream) Write(frame []int16) error {
	if s.writes.Add(1) == s.at {
		s.fault()
	}
	return s.alsaStream.Write(frame)
}

// spin computes for d.
func spin(d time.Duration) {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
	}
}

// crowd keeps the calling goroutine runnable but not run for about d: one
// processor for Go, 16 goroutines computing on it until d is up, and the
// caller yielding to them over and over, each time to the back of the queue.
func crowd(d time.Duration) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	deadline := time.Now().Add(d)
	var spinning sync.WaitGroup
	for range 16 {
		spinning.Go(func() {
			for time.Now().Before(deadline) {
			}
		})
	}
	for time.Now().Before(deadline) {
		runtime.Gosched()
	}
	spinning.Wait()
}

// holdTheLock takes out's lock, says so on locked, and holds it for d.
func holdTheLock(out *Output, d time.Duration, locked chan<- struct{}) {
	out.mu.Lock()
	close(locked)
	time.Sleep(d)
	out.mu.Unlock()
}

// TestTheMixersRegionsAreTheReadersRegions fails if the regions the mixer
// marks are not the ones internal/speakertrace reads.
func TestTheMixersRegionsAreTheReadersRegions(t *testing.T) {
	if regionWrite != speakertrace.RegionWrite || regionPacer != speakertrace.RegionPacer || regionLock != speakertrace.RegionLock {
		t.Fatalf("the mixer marks %q, %q and %q; the reader reads %q, %q and %q", regionWrite, regionPacer, regionLock,
			speakertrace.RegionWrite, speakertrace.RegionPacer, speakertrace.RegionLock)
	}
}

// nopStream is a stream whose writes do nothing.
type nopStream struct{ PlaybackStream }

func (nopStream) Write([]int16) error { return nil }

// TestTheMixersRegionsAllocateNothing fails if marking the mixer's regions
// allocates while a trace runs: a mixer that allocates during a collection's
// mark phase is made to help mark and to wait for the phase to end, so the
// trace would put it in waits it never has without one.
func TestTheMixersRegionsAllocateNothing(t *testing.T) {
	if err := trace.Start(io.Discard); err != nil {
		t.Fatal(err)
	}
	defer trace.Stop()
	out := &Output{}
	frame := make([]int16, 480)
	for name, f := range map[string]func(){
		"the lock":  func() { out.lockForMix(); out.mu.Unlock() },
		"the write": func() { _ = writeFrame(nopStream{}, frame) },
		"the pacer": func() { pacerSleep(0) },
	} {
		if n := testing.AllocsPerRun(100, f); n != 0 {
			t.Errorf("%s's region allocates %v times while a trace runs, want 0", name, n)
		}
	}
}

// regionFault marks an injected fault in the trace, so the test finds the
// fault's cycle among the slow ones a loaded machine makes on its own. Its
// name starts with "speaker.", so the reader counts time in it as time in it.
const regionFault = "speaker.fault"

// TestTheSpeakerTraceSaysWhatKeptTheMixerFromTheDevice fails unless the speaker
// trace, saved on the underrun that a fault in the mixer's path causes on a
// device that plays in real time from an 80 ms buffer, names that fault as
// what kept the mixer from the device: a sleep inside the stream's write,
// computing inside it, the Output's lock held by another goroutine, which it
// names, and Go running other goroutines while the mixer could run. These are
// the instrument's controls: a capture of a real underrun is read only as far
// as these are named right. The fault runs inside regionFault, so a slow
// cycle the machine makes on its own, which can be longer, is not taken for it.
func TestTheSpeakerTraceSaysWhatKeptTheMixerFromTheDevice(t *testing.T) {
	const stall = 150 * time.Millisecond
	for _, tc := range []struct {
		name  string
		fault func(out *Output)
		want  []string
	}{
		{"a sleep in the write", func(*Output) { time.Sleep(stall) }, []string{"a sleep inside " + regionFault}},
		{"computing in the write", func(*Output) { spin(stall) }, []string{"running", "inside " + regionFault}},
		{"the Output's lock held", func(out *Output) {
			locked := make(chan struct{})
			go holdTheLock(out, stall, locked)
			<-locked // the write returns, and the mixer's next take of the lock waits
		}, []string{"the Output's lock", "holdTheLock"}},
		{"no processor free", func(*Output) { crowd(stall) }, []string{"runnable but not run", "inside " + regionFault}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := StartSpeakerTrace(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			st.every = 0 // save on every underrun: a stray one must not take the fault's capture's place
			saved := make(chan string, 64)
			st.saved = func(path string) { saved <- path }
			pcm := newFakePCM(48000, 48000*int(alsaBuffer/time.Millisecond)/1000)
			stream := &injectStream{alsaStream: streamOn(pcm), at: 50}
			var out *Output
			stream.fault = func() { trace.WithRegion(traceRegions, regionFault, func() { tc.fault(out) }) }
			out = NewOutput(OutputConfig{IdleClose: 100 * time.Millisecond, Underrun: st.Underrun, Open: func(string) (PlaybackStream, int, error) {
				return stream, pcm.rate, nil
			}})
			ctx := testContext(t)
			voice := out.Begin(InputVoice)
			if err := voice.WriteAll(ctx, sine(48000, 2*time.Second, 440, 8000), mono48k); err != nil {
				t.Fatal(err)
			}
			if err := voice.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			_ = out.Close()
			st.Stop()
			if stream.writes.Load() < stream.at {
				t.Fatalf("control: the mixer wrote %d frames, fewer than the %d the fault needs", stream.writes.Load(), stream.at)
			}
			close(saved)
			files := 0
			var slow []string
			for path := range saved {
				files++
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				cycles, err := speakertrace.Read(f, 40*time.Millisecond)
				_ = f.Close()
				if err != nil {
					t.Fatalf("read %s: %v", path, err)
				}
				for i := range cycles {
					c := &cycles[i]
					got := c.Classify()
					slow = append(slow, fmt.Sprintf("%v: %s", c.Length().Round(time.Millisecond), got))
					if c.Length() >= stall*4/5 && containsAll(got, tc.want) {
						t.Logf("the fault's cycle: %v: %s; all: %v", c.Length(), got, c.Spent)
						return
					}
				}
			}
			if files == 0 {
				t.Fatal("control: the fault's underrun saved no capture")
			}
			t.Fatalf("no slow cycle of at least %v in %d captures names %q; the slow cycles: %q", stall*4/5, files, tc.want, slow)
		})
	}
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
