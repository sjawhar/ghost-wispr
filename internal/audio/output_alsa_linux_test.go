//go:build linux

package audio

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// pcmRun is one stretch a fakePCM played without a stop: from start, the
// device takes a frame every 1/rate seconds from samples, and the room hears
// each inflight frames after the device took it.
type pcmRun struct {
	start      time.Time
	started    bool
	samples    []int16
	heard      int // frames the room heard: set when the run stops
	stopped    bool
	taken      int // frames a lockstep device took
	startClock int // the lockstep clock when the run started
}

// fakePCM is a mono ALSA playback PCM that plays in real time, as the
// Jabra's does: it holds up to buffer frames, starts once it holds that many
// (ALSA's start threshold), then takes rate frames a second, and a write waits
// while it is full. What it took is heard inflight frames later, as a USB
// device hears what the driver handed to USB: avail counts those frames free,
// delay counts them queued, and only what the device has not taken can be
// rewound. drop discards what it holds and what is in flight. A write after it
// ran dry reports an underrun, -EPIPE, until prepare. With noRewind it rewinds
// nothing, as a PCM that cannot.
type fakePCM struct {
	rate, buffer int
	inflight     int
	noRewind     bool
	// lockstep makes the device take frames only as writes need room, so its
	// time is the stream's, not the clock's (writei).
	lockstep bool

	mu    sync.Mutex
	runs  []*pcmRun
	clock int // frames a lockstep device has taken in all runs
	// fire, on a lockstep device, runs once from inside the write that takes
	// its clock to fireAt; firedAt is the clock then.
	fire    func()
	fireAt  int
	fired   bool
	firedAt int
	xrun    bool
	closed  bool
	xruns   int
	minHeld int // the fewest frames it held at a write once running: the headroom left
}

func newFakePCM(rate, buffer int) *fakePCM {
	return &fakePCM{rate: rate, buffer: buffer, runs: []*pcmRun{{}}, minHeld: buffer}
}

// consumed is how many frames of run the device has taken by now. The caller
// holds mu.
func (p *fakePCM) consumed(run *pcmRun, now time.Time) int {
	if run.stopped {
		return run.heard
	}
	if !run.started {
		return 0
	}
	if p.lockstep {
		return run.taken
	}
	return min(len(run.samples), int(int64(now.Sub(run.start))*int64(p.rate)/int64(time.Second)))
}

// heardBy is how many frames of run the room has heard by now. The caller
// holds mu.
func (p *fakePCM) heardBy(run *pcmRun, now time.Time) int {
	if run.stopped {
		return run.heard
	}
	return max(0, p.consumed(run, now)-p.inflight)
}

// stop ends run, killing what is in flight. The caller holds mu.
func (p *fakePCM) stop(run *pcmRun, now time.Time) {
	if !run.stopped {
		run.heard, run.stopped = p.heardBy(run, now), true
	}
}

// writei writes frames as ALSA's blocking writei does: as much as fits, then
// the rest as the device frees room, starting the device once it is full. In
// lockstep the device takes, at once, as many frames as the write needs room
// for: its time moves only with the writes, so a write that comes late,
// however late, finds the device where the last one left it.
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
		if run.started && held == 0 && !p.lockstep {
			run.heard, run.stopped = len(run.samples), true // what was in flight plays out
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
				run.start, run.started, run.startClock = now, true, p.clock
			}
		}
		if written == frames {
			return written
		}
		if p.lockstep {
			take := min(held, frames-written)
			run.taken += take
			p.clock += take
			if p.fire != nil && !p.fired && p.clock >= p.fireAt {
				p.fired, p.firedAt = true, p.clock
				p.mu.Unlock()
				p.fire()
				p.mu.Lock()
			}
			continue
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

func (p *fakePCM) delay() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	run := p.runs[len(p.runs)-1]
	now := time.Now()
	return len(run.samples) - p.heardBy(run, now), 0
}

func (p *fakePCM) rewindable() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.noRewind {
		return 0
	}
	run := p.runs[len(p.runs)-1]
	return len(run.samples) - p.consumed(run, time.Now())
}

// rewind takes back up to frames that the device has not taken, and reports
// how many it took back.
func (p *fakePCM) rewind(frames int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.noRewind {
		return 0
	}
	run := p.runs[len(p.runs)-1]
	n := min(frames, len(run.samples)-p.consumed(run, time.Now()))
	run.samples = run.samples[:len(run.samples)-n]
	return n
}

func (p *fakePCM) drop() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stop(p.runs[len(p.runs)-1], time.Now())
	return 0
}

// prepare refuses a running PCM with -EBUSY, as the kernel does
// (snd_pcm_pre_prepare): a drop must stop it first.
func (p *fakePCM) prepare() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	run := p.runs[len(p.runs)-1]
	if run.started && !run.stopped {
		return -int(syscall.EBUSY)
	}
	if !run.stopped {
		run.heard, run.stopped = 0, true // a prepared PCM that never started played nothing
	}
	p.runs = append(p.runs, &pcmRun{})
	p.xrun = false
	return 0
}

func (p *fakePCM) close() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stop(p.runs[len(p.runs)-1], time.Now())
	p.closed = true
	return 0
}

func (p *fakePCM) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// heard calls fn with each frame the room has heard and the time it ended.
func (p *fakePCM) heard(fn func(sample int16, end time.Time)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, run := range p.runs {
		for i := range p.heardBy(run, now) {
			fn(run.samples[i], run.start.Add(time.Duration(i+1+p.inflight)*time.Second/time.Duration(p.rate)))
		}
	}
}

// heardInStreamTime calls fn, on a lockstep PCM, with each frame the room
// has heard and the device clock, in frames taken, when it was heard.
func (p *fakePCM) heardInStreamTime(fn func(sample int16, clock int)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, run := range p.runs {
		for i := range p.heardBy(run, time.Time{}) {
			fn(run.samples[i], run.startClock+i+p.inflight)
		}
	}
}

// cancelClock is the clock of a lockstep PCM when fire ran, and whether it
// ran.
func (p *fakePCM) cancelClock() (clock int, fired bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firedAt, p.fired
}

// outputOnPCM is an Output on a fakePCM through the ALSA path, with the
// period and buffer the speaker opens on the device.
func outputOnPCM(t *testing.T, pcm *fakePCM, idle time.Duration) *Output {
	t.Helper()
	out := NewOutput(OutputConfig{IdleClose: idle, Open: func(string) (PlaybackStream, int, error) {
		return streamOn(pcm), pcm.rate, nil
	}})
	t.Cleanup(func() { _ = out.Close() })
	return out
}

// streamOn is the speaker's stream on pcm, mono, with 10 ms periods.
func streamOn(pcm *fakePCM) *alsaStream {
	return newALSAStream(pcm, "fake", alsaParams{rate: pcm.rate, channels: 1, period: pcm.rate / 100, buffer: pcm.buffer})
}

// TestALSAOutputCancelSilencesTheVoiceWithin20ms fails if the room still
// hears the voice more than 20 ms after Cancel on the ALSA path, where the
// device holds alsaBuffer that it has not played, and a few milliseconds more
// are in flight to the speaker, as on USB:
//   - a PCM that rewinds takes back what the device holds, and the chime,
//     which plays on, loses nothing and repeats nothing: exactly the frames
//     ALSA took back play again without the voice;
//   - a PCM that cannot rewind drops what it holds instead;
//   - a PCM whose audio in flight would play on for more than the bound
//     after a rewind is dropped too, which cuts what is in flight.
//
// The voice is positive and the chime negative, so that each sample tells
// which plays in it, the voice ducked under the chime included.
//
// The device runs in lockstep and the Cancel comes from inside its write at
// 200 ms of its clock, so the time is the stream's, counted in frames from
// the Cancel to the last frame of the voice the room hears: a loaded machine
// slows the run but cannot stretch it.
func TestALSAOutputCancelSilencesTheVoiceWithin20ms(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inflight   time.Duration
		noRewind   bool
		exactChime bool // the chime plays every frame once
	}{
		{name: "rewind", inflight: 3 * time.Millisecond, exactChime: true},
		{name: "no rewind, so drop", inflight: 3 * time.Millisecond, noRewind: true},
		{name: "rewind leaves too long a tail, so drop", inflight: 30 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pcm := newFakePCM(48000, 48000*int(alsaBuffer/time.Millisecond)/1000)
			pcm.inflight, pcm.noRewind, pcm.lockstep = int(48*tc.inflight/time.Millisecond), tc.noRewind, true
			out := outputOnPCM(t, pcm, 100*time.Millisecond)
			pcm.fireAt, pcm.fire = 48000/5, func() { out.Cancel(InputVoice) }
			ctx := testContext(t)
			voice, chime := out.Begin(InputVoice), out.Begin(InputChime)
			go func() { _ = voice.WriteAll(ctx, constant(48000, 2*time.Second, 1000), mono48k) }()
			chimed := make(chan error, 1)
			go func() {
				err := chime.WriteAll(ctx, constant(48000, 500*time.Millisecond, -500), mono48k)
				if err == nil {
					err = chime.Drain(ctx)
				}
				chimed <- err
			}()
			if err := <-chimed; err != nil {
				t.Fatalf("the chime: %v", err)
			}
			waitFor(t, "the idle stream to close", pcm.isClosed)

			cancelled, fired := pcm.cancelClock()
			if !fired {
				t.Fatal("control: the device never reached the Cancel")
			}
			lastVoice, chimeFrames := -1, 0
			pcm.heardInStreamTime(func(sample int16, clock int) {
				switch {
				case sample == duckedUnderChime: // voice and chime
					lastVoice = clock
					chimeFrames++
				case sample == -500:
					chimeFrames++
				case sample > 0: // the voice, alone or going down
					lastVoice = clock
				}
			})
			if lastVoice < 0 {
				t.Fatal("control: the room never heard the voice")
			}
			heard := time.Duration(lastVoice+1-cancelled) * time.Second / 48000
			t.Logf("the voice was last heard %v of stream time after Cancel; the room heard %d frames of the chime", heard, chimeFrames)
			if heard > 20*time.Millisecond {
				t.Fatalf("the room heard the voice for %v of stream time after Cancel, want silence within 20 ms", heard)
			}
			if tc.exactChime && chimeFrames != 24000 {
				t.Fatalf("the room heard %d frames of the chime, want each of its 24000 (500 ms) once", chimeFrames)
			}
			if lost := 24000 - chimeFrames; !tc.exactChime && (lost < 0 || lost > pcm.inflight) {
				t.Fatalf("the room heard %d frames of the chime: a drop may lose only the %d in flight", chimeFrames, pcm.inflight)
			}
		})
	}
}

// TestALSAOutputReportsWhatTheRoomHeardOfACancelledSpeech fails if, after a
// Cancel, a playback's Played and its speech's end event do not report what
// the device played of it: the drop keeps from the room only the audio the
// device still held. A Cancel just after the speech's last audio played, as a
// barge-in often lands, takes nothing off and does not mark the speech
// cancelled; a Cancel while the device holds the speech's end takes off what
// it held.
//
// The device runs in lockstep, the speech is queued before the stream opens,
// and the Cancel comes from inside the device's write at a point of its
// clock, so where the Cancel lands in the speech is the stream's, not the
// machine's. The idle close counts wall time, but it cannot come before the
// Cancel either: the stream closes only once the device holds nothing of the
// speech (Output.mix, quiet against queuedFrames), and that is first true in
// the write that takes the clock to the speech's end, inside which the first
// case's Cancel comes. A Cancel at a later clock would race the idle close,
// and a mixer a few milliseconds late would close the stream first.
func TestALSAOutputReportsWhatTheRoomHeardOfACancelledSpeech(t *testing.T) {
	for _, tc := range []struct {
		name      string
		at        int // the device clock, in frames, at the Cancel
		heard     time.Duration
		cancelled bool
	}{
		// The 100 ms speech is frames 0-4799; the device holds 3840, so it
		// has taken 960 when the speech's last frame is written, and 4800
		// once it has taken the speech's last frame.
		{name: "cancel after the speech played out", at: 4800, heard: 100 * time.Millisecond},
		{name: "cancel while the device holds its end", at: 1440, heard: 30 * time.Millisecond, cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pcm := newFakePCM(48000, 48000*int(alsaBuffer/time.Millisecond)/1000)
			pcm.lockstep = true
			gate := make(chan struct{})
			out := NewOutput(OutputConfig{IdleClose: 100 * time.Millisecond, Open: func(string) (PlaybackStream, int, error) {
				<-gate
				return streamOn(pcm), pcm.rate, nil
			}})
			t.Cleanup(func() { _ = out.Close() })
			pcm.fireAt, pcm.fire = tc.at, func() { out.Cancel(InputVoice) }
			ends := make(chan SpeechEvent, 4)
			defer out.OnSpeech(func(e SpeechEvent) {
				if !e.Started {
					ends <- e
				}
			})()
			ctx := testContext(t)
			pb := out.Begin(InputVoice)
			if err := pb.WriteAll(ctx, constant(48000, 100*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			close(gate)
			var end SpeechEvent
			select {
			case end = <-ends:
			case <-time.After(5 * time.Second):
				t.Fatal("no end event")
			}
			waitFor(t, "the idle stream to close", pcm.isClosed)
			if _, fired := pcm.cancelClock(); !fired {
				t.Fatal("control: the device never reached the Cancel")
			}
			heardFrames := 0
			pcm.heardInStreamTime(func(s int16, _ int) {
				if s == 1000 {
					heardFrames++
				}
			})
			heard := time.Duration(heardFrames) * time.Second / 48000
			if heard != tc.heard {
				t.Fatalf("control: the room heard %v of the voice, want %v", heard, tc.heard)
			}
			if played := pb.Played(); played != heard {
				t.Fatalf("the playback reports %v played, the device played %v of it", played, heard)
			}
			if end.Played != heard || end.Cancelled != tc.cancelled {
				t.Fatalf("end event = %+v, want %v played (what the device played), cancelled %t", end, heard, tc.cancelled)
			}
		})
	}
}

// TestALSAOutputCancelKeepsAChimeLongerThanTheHistory fails if a Cancel on a
// device that granted a buffer longer than outputHistory loses the part of the
// chime beyond it: ALSA may grant more than the speaker asks for.
func TestALSAOutputCancelKeepsAChimeLongerThanTheHistory(t *testing.T) {
	pcm := newFakePCM(48000, 48000*150/1000) // 150 ms
	out := outputOnPCM(t, pcm, 100*time.Millisecond)
	ctx := testContext(t)
	voice, chime := out.Begin(InputVoice), out.Begin(InputChime)
	go func() { _ = voice.WriteAll(ctx, constant(48000, 2*time.Second, 1000), mono48k) }()
	chimed := make(chan error, 1)
	go func() {
		err := chime.WriteAll(ctx, constant(48000, 500*time.Millisecond, -500), mono48k)
		if err == nil {
			err = chime.Drain(ctx)
		}
		chimed <- err
	}()
	time.Sleep(250 * time.Millisecond)
	out.Cancel(InputVoice)
	if err := <-chimed; err != nil {
		t.Fatalf("the chime: %v", err)
	}
	waitFor(t, "the idle stream to close", pcm.isClosed)
	chimeFrames := 0
	pcm.heard(func(sample int16, _ time.Time) {
		if sample == duckedUnderChime || sample == -500 {
			chimeFrames++
		}
	})
	if chimeFrames != 24000 {
		t.Fatalf("the room heard %d frames of the chime, want each of its 24000 (500 ms) once", chimeFrames)
	}
}

// duckedUnderChime is a sample of the voice at 1000 ducked under the chime at
// -500.
var duckedUnderChime = int16(math.Round(1000*float64(duckGain))) - 500

// TestALSAStreamLogsWhetherItRewinds fails unless a stream says once, when
// its device first holds audio, whether a Cancel there rewinds or drops, so a
// run on the Jabra shows which it does.
func TestALSAStreamLogsWhetherItRewinds(t *testing.T) {
	for _, tc := range []struct {
		noRewind bool
		want     string
	}{
		{want: "rewind=true"},
		{noRewind: true, want: "rewind=false"},
	} {
		var logs strings.Builder
		var mu sync.Mutex
		original := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
		pcm := newFakePCM(48000, 960)
		pcm.noRewind = tc.noRewind
		stream := streamOn(pcm)
		frame := make([]int16, 480)
		for range 4 {
			if err := stream.Write(frame); err != nil {
				t.Fatal(err)
			}
		}
		slog.SetDefault(original)
		mu.Lock()
		got := logs.String()
		mu.Unlock()
		if n := strings.Count(got, "speaker stream"); n != 1 || !strings.Contains(got, tc.want) {
			t.Errorf("a stream that rewinds: %t logged %q, want one speaker stream line with %s", !tc.noRewind, got, tc.want)
		}
	}
}

// TestALSAStreamLogsTheWayEachCancelTookBack fails unless each Cancel's take-
// back says whether it rewound or dropped: a device that rewinds, as the
// stream logs at open, can still leave more in flight than a period, and then
// the cancel drops, so only this line shows what the Jabra does.
//
// The device runs in lockstep: it takes frames only as the writes need room,
// so at the Cancel it has taken 960 frames and holds 960 however late the
// test's writes come. A device that plays in real time runs dry when a loaded
// machine wakes the test late, and a device restarted after the underrun holds
// only frames it has not played, which a Cancel rewinds.
func TestALSAStreamLogsTheWayEachCancelTookBack(t *testing.T) {
	for _, tc := range []struct {
		inflight int
		want     string
	}{
		{inflight: 144, want: "way=rewind"},
		{inflight: 1440, want: "way=drop"},
	} {
		var logs strings.Builder
		var mu sync.Mutex
		original := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
		pcm := newFakePCM(48000, 960)
		pcm.inflight, pcm.lockstep = tc.inflight, true
		stream := streamOn(pcm)
		frame := make([]int16, 480)
		for range 4 {
			if err := stream.Write(frame); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := stream.Drop(); err != nil {
			t.Fatal(err)
		}
		slog.SetDefault(original)
		mu.Lock()
		got := logs.String()
		mu.Unlock()
		if n := strings.Count(got, "speaker cancel"); n != 1 || !strings.Contains(got, tc.want) {
			t.Errorf("a cancel with %d frames in flight logged %q, want one speaker cancel line with %s", tc.inflight, got, tc.want)
		}
	}
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

// TestALSAStreamLogsEveryUnderrunItRecoversFrom fails if a write after an
// underrun gives up instead of preparing the PCM and playing on, or if the
// underrun is not counted, or if any underrun goes unlogged. The Pi's
// playback check reads these lines: a reader of /proc/asound can miss a short
// underrun between its polls, so the count it trusts is the one each line
// carries.
func TestALSAStreamLogsEveryUnderrunItRecoversFrom(t *testing.T) {
	var logs strings.Builder
	var mu sync.Mutex
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
	defer slog.SetDefault(original)

	pcm := newFakePCM(48000, 960) // two writes fill it and start it
	stream := streamOn(pcm)
	frame := make([]int16, 480)
	for i := range frame {
		frame[i] = 7
	}
	for run := range 3 {
		for range 2 {
			if err := stream.Write(frame); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(50 * time.Millisecond) // the device runs dry
		if err := stream.Write(frame); err != nil {
			t.Fatalf("a write after underrun %d = %v, want it to recover and play", run+1, err)
		}
		if got := stream.underruns(); got != run+1 {
			t.Fatalf("underruns counted = %d, want %d", got, run+1)
		}
	}
	mu.Lock()
	got := logs.String()
	mu.Unlock()
	if n := strings.Count(got, `msg="speaker underrun"`); n != 3 {
		t.Errorf("3 underruns logged %d lines, want one each: %q", n, got)
	}
	for _, want := range []string{"underruns=1", "underruns=2", "underruns=3", "device=fake"} {
		if !strings.Contains(got, want) {
			t.Errorf("the underrun lines lack %s: %q", want, got)
		}
	}
}

// TestOutputPlaysThroughALSAWithoutPortAudio fails if the speaker needs
// PortAudio to play. PortAudio's initialization opens every capture PCM, the
// Jabra's microphone included, so it runs only inside a microphone start,
// under a red light: a unit restarted paused has none, and the speaker must
// still speak (A8). It plays through ALSA's null PCM here, as it plays through
// the Jabra's playback PCM on the unit, and counts PortAudio's
// initializations, which must be none. The null PCM never makes a write wait,
// so the Output must keep itself to real time: it runs at most outputAhead
// ahead.
func TestOutputPlaysThroughALSAWithoutPortAudio(t *testing.T) {
	originalInitialize := paInitialize
	t.Cleanup(func() { paInitialize = originalInitialize })
	var initialized atomic.Int32
	paInitialize = func() error { initialized.Add(1); return nil }
	out := NewOutput(OutputConfig{Device: "null"})
	t.Cleanup(func() { _ = out.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	began := time.Now()
	pb := out.Begin(InputVoice)
	if err := pb.WriteAll(ctx, constant(16000, 300*time.Millisecond, 1000), AudioFormat{SampleRate: 16000, Channels: 1}); err != nil {
		t.Fatalf("WriteAll through ALSA = %v, want it played", err)
	}
	if err := pb.Drain(ctx); err != nil {
		t.Fatalf("Drain through ALSA = %v, want it played", err)
	}
	if played := pb.Played(); played != 300*time.Millisecond {
		t.Fatalf("played %v, want 300 ms", played)
	}
	if took := time.Since(began); took < 300*time.Millisecond-outputAhead-outputFrame {
		t.Fatalf("300 ms of audio played in %v: the Output ran ahead of real time", took)
	}
	if n := initialized.Load(); n != 0 {
		t.Fatalf("the speaker initialized PortAudio %d times to play, want none", n)
	}
	// Control: the count sees an initialization.
	if err := InitializePortAudio(); err != nil || initialized.Load() != 1 {
		t.Fatalf("control: InitializePortAudio = %v, counted %d initializations, want 1", err, initialized.Load())
	}
}

// replaySpy is a speaker stream on the ALSA stream code that counts, after a
// Drop, the voiced samples of the voice (above the chime's level) in the
// writes that mix again what the Drop took back, and when the last came.
type replaySpy struct {
	base         *alsaStream
	mu           sync.Mutex
	left, voiced int
	last         time.Time
	done         chan struct{}
}

func (s *replaySpy) bufferFrames() int { return s.base.bufferFrames() }
func (s *replaySpy) queuedFrames() int { return s.base.queuedFrames() }
func (s *replaySpy) Close() error      { return s.base.Close() }

func (s *replaySpy) Write(frame []int16) error {
	err := s.base.Write(frame)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.left > 0 {
		for _, v := range frame {
			if v > 100 {
				s.voiced++
				s.last = time.Now()
			}
		}
		if s.left -= len(frame); s.left <= 0 {
			close(s.done)
		}
	}
	return err
}

func (s *replaySpy) Drop() (int, error) {
	n, err := s.base.Drop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.left = n; n == 0 {
		close(s.done)
	}
	return n, err
}

// TestASpeechEndsOnlyOnceTheDeviceCannotGiveItBack fails if the voice's
// speech is reported ended while the device still holds some of its audio,
// as an ALSA buffer granted longer than speechEndHold does: a Cancel of the
// chime then gave that audio back and the mixer played it again after the
// speech's end, and a Cancel of the voice, no longer speaking, left it in
// (Sol's review of #133 at f398ca8 found 960 samples of a cancelled voice
// replayed with a 150 ms buffer). The 80 ms buffer is the control: it holds
// no voice when its speech ends, before or after this fix.
func TestASpeechEndsOnlyOnceTheDeviceCannotGiveItBack(t *testing.T) {
	for _, buffer := range []int{80, 150} {
		for _, cancelVoice := range []bool{false, true} {
			t.Run(fmt.Sprintf("a %d ms buffer, the voice cancelled %v", buffer, cancelVoice), func(t *testing.T) {
				pcm := newFakePCM(48000, 48*buffer)
				spy := &replaySpy{base: streamOn(pcm), done: make(chan struct{})}
				out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return spy, 48000, nil }})
				t.Cleanup(func() { _ = out.Close() })
				ends := make(chan SpeechEvent, 4)
				defer out.OnSpeech(func(e SpeechEvent) {
					if e.Input == InputVoice && !e.Started {
						ends <- e
					}
				})()
				ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
				defer cancel()
				chime := out.Begin(InputChime)
				go func() { _ = chime.WriteAll(ctx, constant(48000, 2*time.Second, 7), mono48k) }()
				waitFor(t, "the device to play the chime", func() bool {
					pcm.mu.Lock()
					defer pcm.mu.Unlock()
					return pcm.runs[len(pcm.runs)-1].started
				})
				if err := out.Begin(InputVoice).WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
					t.Fatal(err)
				}
				var end SpeechEvent
				select {
				case end = <-ends:
				case <-ctx.Done():
					t.Fatal("the voice's speech never ended")
				}
				pcm.mu.Lock()
				run, held := pcm.runs[len(pcm.runs)-1], 0
				for _, v := range run.samples[pcm.consumed(run, time.Now()):] {
					if v > 100 {
						held++
					}
				}
				pcm.mu.Unlock()
				if held > 0 {
					t.Fatalf("the speech ended while the device still held %d samples of it", held)
				}
				if cancelVoice {
					out.Cancel(InputVoice)
				}
				out.Cancel(InputChime)
				select {
				case <-spy.done:
				case <-ctx.Done():
					t.Fatal("the replay never ended")
				}
				spy.mu.Lock()
				last, replayed := spy.last, spy.voiced
				spy.mu.Unlock()
				if replayed > 0 {
					t.Fatalf("the mixer played %d samples of the ended voice again, the last %v after its end", replayed, last.Sub(end.At))
				}
			})
		}
	}
}
