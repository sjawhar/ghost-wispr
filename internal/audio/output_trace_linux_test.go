//go:build linux

package audio

import (
	"os"
	"runtime"
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

// TestTheSpeakerTraceSaysWhatKeptTheMixerFromTheDevice fails unless the speaker
// trace, saved on the underrun that a fault in the mixer's path causes on a
// device that plays in real time from an 80 ms buffer, names that fault as
// what kept the mixer from the device: a sleep inside the stream's write,
// computing inside it, and the Output's lock held by another goroutine, which
// it names. These are the instrument's controls: a capture of a real underrun
// is read only as far as these are named right.
func TestTheSpeakerTraceSaysWhatKeptTheMixerFromTheDevice(t *testing.T) {
	const stall = 150 * time.Millisecond
	for _, tc := range []struct {
		name  string
		fault func(out *Output)
		want  []string
	}{
		{"a sleep in the write", func(*Output) { time.Sleep(stall) }, []string{"a sleep inside speaker.write"}},
		{"computing in the write", func(*Output) { spin(stall) }, []string{"running", "inside speaker.write"}},
		{"the Output's lock held", func(out *Output) {
			locked := make(chan struct{})
			go holdTheLock(out, stall, locked)
			<-locked // the write returns, and the mixer's next take of the lock waits
		}, []string{"the Output's lock", "holdTheLock"}},
		{"no processor free", func(*Output) { crowd(stall) }, []string{"runnable but not run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := StartSpeakerTrace(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			st.every = 0 // save on every underrun: a stray one must not take the fault's capture's place
			saved := make(chan string, 16)
			st.saved = func(path string) { saved <- path }
			pcm := newFakePCM(48000, 48000*int(alsaBuffer/time.Millisecond)/1000)
			stream := &injectStream{alsaStream: streamOn(pcm), at: 50}
			var out *Output
			stream.fault = func() { tc.fault(out) }
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
			var best speakertrace.Cycle
			files := 0
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
				for _, c := range cycles {
					if c.Length() > best.Length() {
						best = c
					}
				}
			}
			if files == 0 {
				t.Fatal("control: the fault's underrun saved no capture")
			}
			bucket, spent := best.Dominant()
			got := best.Classify()
			t.Logf("the longest cycle in %d captures: %v; most of it %s (%v): %s; all: %v", files, best.Length(), bucket, spent, got, best.Spent)
			if best.Length() < stall*4/5 {
				t.Fatalf("the longest slow cycle took %v, want the %v the fault held the mixer", best.Length(), stall)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("the capture says %q, want it to name %q", got, want)
				}
			}
		})
	}
}
