//go:build linux

package audio

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// pcmRun is one stretch a fakePCM played without a stop: from start, a frame
// every 1/rate seconds, the first played of samples.
type pcmRun struct {
	start   time.Time
	started bool
	samples []int16
	played  int // frames it played: set when the run stops
	stopped bool
}

// fakePCM is a mono ALSA playback PCM that plays in real time, as the
// Jabra's does: it holds up to buffer frames, starts playing once it holds
// that many (ALSA's start threshold), then plays rate frames a second, and a
// write waits while it is full. A write after it ran dry reports an underrun,
// -EPIPE, until prepare. drop discards what it has not played.
type fakePCM struct {
	rate, buffer int

	mu      sync.Mutex
	runs    []*pcmRun
	xrun    bool
	closed  bool
	xruns   int
	minHeld int // the fewest frames it held at a write once running: the headroom left
}

func newFakePCM(rate, buffer int) *fakePCM {
	return &fakePCM{rate: rate, buffer: buffer, runs: []*pcmRun{{}}, minHeld: buffer}
}

// consumed is how many frames of run have played by now. The caller holds mu.
func (p *fakePCM) consumed(run *pcmRun, now time.Time) int {
	if run.stopped {
		return run.played
	}
	if !run.started {
		return 0
	}
	return min(len(run.samples), int(int64(now.Sub(run.start))*int64(p.rate)/int64(time.Second)))
}

// writei writes frames as ALSA's blocking writei does: as much as fits, then
// the rest as the device frees room, starting the device once it is full.
func (p *fakePCM) writei(interleaved []int16, frames int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	written := 0
	for {
		if p.xrun {
			if written > 0 {
				return written
			}
			return -int(syscall.EPIPE)
		}
		run := p.runs[len(p.runs)-1]
		now := time.Now()
		held := len(run.samples) - p.consumed(run, now)
		if run.started && held == 0 {
			run.played, run.stopped = len(run.samples), true
			p.xrun = true
			p.xruns++
			continue
		}
		if run.started {
			p.minHeld = min(p.minHeld, held)
		}
		if fit := min(p.buffer-held, frames-written); fit > 0 {
			run.samples = append(run.samples, interleaved[written:written+fit]...)
			written += fit
			if !run.started && len(run.samples) >= p.buffer {
				run.start, run.started = now, true
			}
		}
		if written == frames {
			return written
		}
		wait := time.Duration(frames-written) * time.Second / time.Duration(p.rate)
		p.mu.Unlock()
		time.Sleep(wait)
		p.mu.Lock()
	}
}

func (p *fakePCM) recover(rc int) int {
	if rc != -int(syscall.EPIPE) {
		return rc
	}
	return p.prepare()
}

func (p *fakePCM) avail() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	run := p.runs[len(p.runs)-1]
	return p.buffer - (len(run.samples) - p.consumed(run, time.Now()))
}

func (p *fakePCM) drop() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	run := p.runs[len(p.runs)-1]
	run.played, run.stopped = p.consumed(run, time.Now()), true
	return 0
}

func (p *fakePCM) prepare() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if run := p.runs[len(p.runs)-1]; !run.stopped {
		run.played, run.stopped = p.consumed(run, time.Now()), true
	}
	p.runs = append(p.runs, &pcmRun{})
	p.xrun = false
	return 0
}

func (p *fakePCM) close() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if run := p.runs[len(p.runs)-1]; !run.stopped {
		run.played, run.stopped = p.consumed(run, time.Now()), true
	}
	p.closed = true
	return 0
}

func (p *fakePCM) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// heard calls fn with each frame the PCM has played and the time it ended.
func (p *fakePCM) heard(fn func(sample int16, end time.Time)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, run := range p.runs {
		for i := range p.consumed(run, now) {
			fn(run.samples[i], run.start.Add(time.Duration(i+1)*time.Second/time.Duration(p.rate)))
		}
	}
}

// outputOnPCM is an Output on a fakePCM through the ALSA path, with the
// period and buffer the speaker opens on the device.
func outputOnPCM(t *testing.T, pcm *fakePCM, idle time.Duration) *Output {
	t.Helper()
	out := NewOutput(OutputConfig{IdleClose: idle, Open: func(string) (PlaybackStream, int, error) {
		return newALSAStream(pcm, 1, pcm.buffer), pcm.rate, nil
	}})
	t.Cleanup(func() { _ = out.Close() })
	return out
}

// TestALSAOutputCancelSilencesTheVoiceWithin20ms fails if the room still
// hears the voice more than 20 ms after Cancel on the ALSA path, where the
// device holds 40 ms that it has not played (alsaBuffer): the frames it holds
// must be dropped, not played out. The chime, which plays on, must lose
// nothing to the drop: what the device held of it plays again.
func TestALSAOutputCancelSilencesTheVoiceWithin20ms(t *testing.T) {
	pcm := newFakePCM(48000, 48000*int(alsaBuffer/time.Millisecond)/1000)
	out := outputOnPCM(t, pcm, 100*time.Millisecond)
	ctx := testContext(t)
	voice, chime := out.Begin(InputVoice), out.Begin(InputChime)
	go func() { _ = voice.WriteAll(ctx, constant(48000, 2*time.Second, 1000), mono48k) }()
	chimed := make(chan error, 1)
	go func() {
		err := chime.WriteAll(ctx, constant(48000, 500*time.Millisecond, 500), mono48k)
		if err == nil {
			err = chime.Drain(ctx)
		}
		chimed <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancelled := time.Now()
	out.Cancel(InputVoice)
	if err := <-chimed; err != nil {
		t.Fatalf("the chime: %v", err)
	}
	waitFor(t, "the idle stream to close", pcm.isClosed)

	var lastVoice time.Time
	chimeFrames := 0
	pcm.heard(func(sample int16, end time.Time) {
		switch sample {
		case 1500: // voice and chime
			lastVoice = end
			chimeFrames++
		case 1000:
			lastVoice = end
		case 500:
			chimeFrames++
		}
	})
	if lastVoice.IsZero() {
		t.Fatal("control: the device never played the voice")
	}
	if heard := lastVoice.Sub(cancelled); heard > 20*time.Millisecond {
		t.Fatalf("the device played the voice for %v after Cancel, want silence within 20 ms", heard)
	}
	// avail and drop are two calls, and the device plays on between them, so
	// up to a few frames it played can be written again; the 40 ms it held
	// are 1920 frames.
	if chimeFrames < 24000 || chimeFrames > 24000+48 {
		t.Fatalf("the device played %d frames of the chime, want all 24000 (500 ms): the drop lost what it held of the chime", chimeFrames)
	}
	t.Logf("the voice was last heard %v after Cancel", lastVoice.Sub(cancelled))
}

// TestALSAOutputKeepsUpUnderLoad measures underruns on the ALSA path with
// the period and buffer the speaker uses, under load like the Pi's: four
// processors, two of them kept busy making garbage for the collector, while
// a 24 kHz voice is resampled and mixed with a chime for three seconds. It
// fails on any underrun, each a click in the room.
func TestALSAOutputKeepsUpUnderLoad(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	var stop atomic.Bool
	var busy sync.WaitGroup
	for range 2 {
		busy.Add(1)
		go func() {
			defer busy.Done()
			var keep [][]byte
			for !stop.Load() {
				keep = append(keep, make([]byte, 64<<10))
				if len(keep) > 64 {
					keep = keep[:0]
				}
			}
		}()
	}
	defer func() { stop.Store(true); busy.Wait() }()

	pcm := newFakePCM(48000, 48000*int(alsaBuffer/time.Millisecond)/1000)
	out := outputOnPCM(t, pcm, 100*time.Millisecond)
	ctx := testContext(t)
	voice, chime := out.Begin(InputVoice), out.Begin(InputChime)
	done := make(chan error, 2)
	go func() {
		done <- voice.WriteAll(ctx, sine(24000, 3*time.Second, 440, 8000), AudioFormat{SampleRate: 24000, Channels: 1})
	}()
	go func() { done <- chime.WriteAll(ctx, sine(48000, 3*time.Second, 880, 4000), mono48k) }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := voice.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	frames := 0
	pcm.heard(func(int16, time.Time) { frames++ })
	if frames < 3*48000-pcm.buffer {
		t.Fatalf("control: the device played %d frames, want the three seconds of audio", frames)
	}
	pcm.mu.Lock()
	xruns, headroom := pcm.xruns, time.Duration(pcm.minHeld)*time.Second/time.Duration(pcm.rate)
	pcm.mu.Unlock()
	t.Logf("underruns: %d; least audio the device held at a write: %v of %v", xruns, headroom, alsaBuffer)
	if xruns > 0 {
		t.Fatalf("%d underruns in three seconds under load", xruns)
	}
}

// TestALSAStreamRecoversFromAnUnderrun fails if a write after an underrun
// gives up instead of preparing the PCM and playing on, or if the underrun is
// not counted.
func TestALSAStreamRecoversFromAnUnderrun(t *testing.T) {
	pcm := newFakePCM(48000, 960) // two writes fill it and start it
	stream := newALSAStream(pcm, 1, pcm.buffer)
	frame := make([]int16, 480)
	for i := range frame {
		frame[i] = 7
	}
	for range 2 {
		if err := stream.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond) // the device runs dry
	if err := stream.Write(frame); err != nil {
		t.Fatalf("a write after an underrun = %v, want it to recover and play", err)
	}
	if got := stream.underruns(); got != 1 {
		t.Fatalf("underruns counted = %d, want 1", got)
	}
}

// TestOutputPlaysThroughALSAWithoutPortAudio fails if the speaker needs
// PortAudio to play. PortAudio's initialization opens every capture PCM, the
// Jabra's microphone included, so it runs only inside a microphone start,
// under a red light: a unit restarted paused has none, and the speaker must
// still speak (A8). It plays through ALSA's null PCM here, as it plays through
// the Jabra's playback PCM on the unit, with PortAudio not initialized. The
// null PCM never makes a write wait, so the Output must keep itself to real
// time: it runs at most outputAhead ahead.
func TestOutputPlaysThroughALSAWithoutPortAudio(t *testing.T) {
	_ = TerminatePortAudio() // whatever an earlier test initialized
	out := NewOutput(OutputConfig{Device: "null"})
	t.Cleanup(func() { _ = out.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	began := time.Now()
	pb := out.Begin(InputVoice)
	if err := pb.WriteAll(ctx, constant(16000, 300*time.Millisecond, 1000), AudioFormat{SampleRate: 16000, Channels: 1}); err != nil {
		t.Fatalf("WriteAll with PortAudio not initialized = %v, want it played through ALSA", err)
	}
	if err := pb.Drain(ctx); err != nil {
		t.Fatalf("Drain with PortAudio not initialized = %v, want it played through ALSA", err)
	}
	if played := pb.Played(); played != 300*time.Millisecond {
		t.Fatalf("played %v, want 300 ms", played)
	}
	if took := time.Since(began); took < 300*time.Millisecond-outputAhead-outputFrame {
		t.Fatalf("300 ms of audio played in %v: the Output ran ahead of real time", took)
	}
}
