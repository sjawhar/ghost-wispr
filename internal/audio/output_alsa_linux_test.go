//go:build linux

package audio

import (
	"cmp"
	"context"
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
	// suspended makes a write report -ESTRPIPE, as after a system suspend,
	// until a resume or a prepare; resumes says a resume succeeds and loses
	// nothing, else the suspend stopped the device, cancelling what was in
	// flight as a USB speaker's driver does, and only a prepare, which
	// discards what it held, brings it back. suspendHeld is what it held
	// then, which its status reports: not what was in flight.
	suspended, resumes bool
	suspendHeld        int
	// delayFails and statusFails make those queries report -EIO;
	// dropProgress is how many frames a lockstep device plays between the
	// status a counted drop reads and the drop.
	delayFails, statusFails bool
	dropProgress            int
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
		if p.suspended {
			if written > 0 {
				return written
			}
			return -int(syscall.ESTRPIPE)
		}
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

// recover recovers as snd_pcm_recover does: an underrun by a prepare; a
// suspend by a resume, else by a prepare, which discards what the PCM held
// and still reports success.
func (p *fakePCM) recover(rc int) int {
	switch rc {
	case -int(syscall.EPIPE):
		return p.prepare()
	case -int(syscall.ESTRPIPE):
		if p.resume() >= 0 {
			return 0
		}
		return p.prepare()
	}
	return rc
}

// suspend suspends the PCM, as the system does: what it holds stays until a
// resume, or is lost to a prepare. A PCM that resumes plays on; one that
// cannot stopped, and what was in flight is lost with it.
func (p *fakePCM) suspend() {
	p.mu.Lock()
	defer p.mu.Unlock()
	run := p.runs[len(p.runs)-1]
	now := time.Now()
	p.suspended, p.suspendHeld = true, len(run.samples)-p.consumed(run, now)
	if !p.resumes {
		run.heard, run.stopped = p.heardBy(run, now), true
	}
}

func (p *fakePCM) resume() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.suspended {
		return 0
	}
	if !p.resumes {
		return -int(syscall.ENOSYS)
	}
	p.suspended = false
	return 0
}

// status is one snapshot as the kernel's status gives it: a delay only while
// the PCM runs (zero before it starts, and when suspended or stopped), and
// what its buffer has free in every state. statusFails makes it fail.
func (p *fakePCM) status() pcmStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statusLocked(time.Now())
}

func (p *fakePCM) statusLocked(now time.Time) pcmStatus {
	if p.statusFails {
		return pcmStatus{rc: -int(syscall.EIO)}
	}
	run := p.runs[len(p.runs)-1]
	if p.suspended {
		return pcmStatus{suspended: true, avail: p.buffer - p.suspendHeld}
	}
	st := pcmStatus{avail: p.buffer - (len(run.samples) - p.consumed(run, now))}
	if run.started && !run.stopped {
		st.running, st.delay = true, len(run.samples)-p.heardBy(run, now)
	}
	return st
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
	if p.delayFails {
		return 0, -int(syscall.EIO)
	}
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

// dropAfterStatus reads one status snapshot, lets a lockstep device play
// dropProgress frames, and drops: it discards what it holds and what is in
// flight, and reports the time those frames took as the time between the two
// calls. In that time the room hears dropProgress more frames, of what was in
// flight, while the device takes as many from what it holds, if it holds any:
// a rewind may have taken all of that back.
func (p *fakePCM) dropAfterStatus() (pcmStatus, time.Duration, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	st := p.statusLocked(now)
	if st.rc < 0 {
		return st, 0, 0
	}
	run := p.runs[len(p.runs)-1]
	heard := p.heardBy(run, now)
	var between time.Duration
	if n := p.dropProgress; n > 0 && p.lockstep && run.started && !run.stopped {
		take := min(n, len(run.samples)-run.taken)
		run.taken += take
		p.clock += take
		run.heard, run.stopped = min(run.taken, heard+n), true
		between = time.Duration(n) * time.Second / time.Duration(p.rate)
	}
	p.stop(run, now)
	return st, between, 0
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
	p.xrun, p.suspended = false, false
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
// are in flight to the speaker, as on USB, or if the chime, which plays on,
// is heard for more or fewer frames than it has:
//   - a PCM that rewinds takes back what the device holds: exactly the frames
//     ALSA took back play again without the voice;
//   - a PCM that cannot rewind drops what it holds instead;
//   - a PCM whose audio in flight would play on for more than the bound
//     after a rewind is dropped too, which cuts what is in flight: that plays
//     again too, without the voice, as the room would not hear it otherwise.
//
// The voice is positive and the chime negative, so that each sample tells
// which plays in it, the voice ducked under the chime included. The chime is
// a constant, so this counts its frames, under the voice's duck: it cannot
// tell a frame played twice from another lost, nor their order (Sol's review
// of #148 at fd1d2b6). TestACancelLosesNoneOfAnotherInputsAudio plays a ramp,
// and checks each frame once, in order, in the same cases.
//
// The device runs in lockstep and the Cancel comes from inside its write at
// 200 ms of its clock, so the time is the stream's, counted in frames from
// the Cancel to the last frame of the voice the room hears: a loaded machine
// slows the run but cannot stretch it.
func TestALSAOutputCancelSilencesTheVoiceWithin20ms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inflight time.Duration
		noRewind bool
	}{
		{name: "rewind", inflight: 3 * time.Millisecond},
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
			// The voice's first audio is queued before anything else, so a
			// goroutine a loaded machine runs late cannot leave the voice
			// silent until the Cancel.
			if err := voice.Write(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
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
			if chimeFrames != 24000 {
				t.Fatalf("the room heard %d frames of the chime, want its 24000 (500 ms)", chimeFrames)
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

// stoppedLevel is the level of the input a Cancel stops in the tests that
// read which frames of another input the room heard: that input is a ramp,
// 1, 2, 3 ..., below it, so each heard sample is the ramp's value, plus
// stoppedLevel while the stopped input played with it.
const stoppedLevel = 16384

// rampPCM is n frames of the ramp 1, 2, ... n, 48 kHz mono PCM16.
func rampPCM(n int) []byte {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(i + 1)
	}
	return pcm16Bytes(s)
}

// rampHeard is the ramp's values the room heard, in order.
func rampHeard(pcm *fakePCM) []int {
	var got []int
	pcm.heardInStreamTime(func(sample int16, _ int) {
		v := int(sample)
		if v >= stoppedLevel {
			v -= stoppedLevel
		}
		if v > 0 {
			got = append(got, v)
		}
	})
	return got
}

// wantRamp fails t unless got is the ramp 1, 2, ... n: each frame once, in
// order.
func wantRamp(t *testing.T, got []int, n int) {
	t.Helper()
	for i, v := range got {
		if v != i+1 {
			t.Fatalf("the room's frame %d of the ramp is its %d, want %d: %d frames heard, want each of %d once, in order", i, v, i+1, len(got), n)
		}
	}
	if len(got) != n {
		t.Fatalf("the room heard %d frames of the ramp, want each of its %d once, in order", len(got), n)
	}
}

// twistPCM is a fakePCM with what the plain one lacks: writes of at most
// partial frames; a hook after its first write that takes audio; with
// resuspend, a second suspend right after the first recovery's prepare, while
// the Output replays; with split, a device that plays split frames between
// a delay query and the next avail query, then suspends, as a pause of the
// process between two queries would let it; with statusAt, a suspend just
// before the first status read once the device's clock reaches statusAt,
// after a write the PCM took whole; and with dropSuspend, a suspend just
// before a drop's status read, after the delay a Cancel reads, which also
// ends a status failure.
type twistPCM struct {
	*fakePCM
	partial     int
	afterFirst  func()
	wrote       bool
	resuspend   bool
	rearmed     bool
	split       int
	splitArmed  bool
	splitDone   bool
	statusAt    int
	dropSuspend bool
}

func (p *twistPCM) status() pcmStatus {
	p.mu.Lock()
	armed := p.statusAt > 0 && !p.fired && p.clock >= p.statusAt
	if armed {
		p.fired, p.firedAt = true, p.clock
	}
	p.mu.Unlock()
	if armed {
		p.suspend()
	}
	return p.fakePCM.status()
}

func (p *twistPCM) dropAfterStatus() (pcmStatus, time.Duration, int) {
	if p.dropSuspend {
		p.dropSuspend = false
		p.mu.Lock()
		p.statusFails = false
		p.mu.Unlock()
		p.suspend()
	}
	return p.fakePCM.dropAfterStatus()
}

func (p *twistPCM) writei(s []int16, n int) int {
	if p.partial > 0 {
		n = min(n, p.partial)
	}
	rc := p.fakePCM.writei(s, n)
	if rc > 0 && !p.wrote {
		p.wrote = true
		if p.afterFirst != nil {
			p.afterFirst()
		}
	}
	return rc
}

func (p *twistPCM) prepare() int {
	rc := p.fakePCM.prepare()
	if rc >= 0 && p.resuspend && !p.rearmed {
		p.mu.Lock()
		p.fired, p.fireAt, p.rearmed = false, p.clock+1, true
		p.mu.Unlock()
	}
	return rc
}

func (p *twistPCM) delay() (int, int) {
	d, rc := p.fakePCM.delay()
	p.mu.Lock()
	p.splitArmed = p.split > 0 && !p.splitDone && p.clock >= 960 && !p.suspended
	p.mu.Unlock()
	return d, rc
}

func (p *twistPCM) avail() int {
	p.mu.Lock()
	armed := p.splitArmed
	p.splitArmed = false
	if armed {
		run := p.runs[len(p.runs)-1]
		n := min(p.split, len(run.samples)-run.taken)
		run.taken += n
		p.clock += n
		p.splitDone = true
	}
	p.mu.Unlock()
	avail := p.fakePCM.avail()
	if armed {
		p.suspend()
		p.mu.Lock()
		p.fired = true
		p.mu.Unlock()
	}
	return avail
}

// TestACancelLosesNoneOfAnotherInputsAudio fails if a Cancel of one input
// loses, repeats or reorders a frame of another input, which plays on, as a
// Cancel of the chime alone must not touch the voice. With 30 ms in flight to
// the speaker, or a PCM that cannot rewind, the stream drops the PCM, which
// also cuts what is in flight: that plays again, from the drop's own count of
// what it cut, less what the device played before the drop (Sol's reviews of
// #133 at 4b7f919, all 960 frames of the voice lost, and of #148 at c96723a:
// a delay read before the device played 96 more frames repeated them). Before
// the PCM starts its status reports no delay, so the drop counts what its
// buffer holds (Sol's review of #148 at fd1d2b6: 480 frames lost). When the
// stream cannot read the delay or the status to count a drop by, it drops
// nothing, and what it could not take back plays out, also when the stream
// would close the moment it is quiet (Sol's review of #148 at fd1d2b6: the
// idle close cut 960 frames). A system suspend after the delay was read and
// before the drop's status cancelled what was in flight, which the drop and
// its prepare hide from the next write's recovery, so the drop counts it too
// (Sol's review of #148 at 9b80022: 1,440 frames lost); when that count is
// unknown the stream fails, so the Drain reports an error, rather than play
// on with audio gone. The 3 ms case rewinds.
func TestACancelLosesNoneOfAnotherInputsAudio(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		inflight                time.Duration
		noRewind                bool
		progress                int
		delayFails, statusFails bool
		idle                    time.Duration // IdleClose: 100 ms if 0
		prepared                bool          // the Cancel comes after the first write, before the PCM starts
		dropSuspend             bool          // suspended before the drop's status, which a status failure ends
		frames                  int           // the kept input's frames: 4800 if 0
	}{
		{name: "rewind, 3 ms in flight", inflight: 3 * time.Millisecond},
		{name: "no rewind, so drop", inflight: 3 * time.Millisecond, noRewind: true},
		{name: "30 ms in flight, so drop", inflight: 30 * time.Millisecond},
		{name: "30 ms in flight, the device plays 96 frames before the drop", inflight: 30 * time.Millisecond, progress: 96},
		{name: "30 ms in flight, the delay query fails", inflight: 30 * time.Millisecond, delayFails: true},
		{name: "30 ms in flight, the delay query fails, and the stream closes once quiet", inflight: 30 * time.Millisecond, delayFails: true, idle: time.Nanosecond},
		{name: "30 ms in flight, the status query fails", inflight: 30 * time.Millisecond, statusFails: true},
		{name: "no rewind, cancelled before the PCM starts", noRewind: true, prepared: true},
		{name: "30 ms in flight, suspended before the drop's status", inflight: 30 * time.Millisecond, dropSuspend: true},
		// A 200 ms speech, the most the input queues: the mixer has taken
		// 5280 of its frames at the Cancel, so it still plays when the drop
		// fails the stream.
		{name: "30 ms in flight, suspended before the drop's status, in flight not counted", inflight: 30 * time.Millisecond, statusFails: true, dropSuspend: true, frames: 9600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pcm := newFakePCM(48000, 48000*int(alsaBuffer/time.Millisecond)/1000)
			pcm.inflight, pcm.lockstep, pcm.noRewind, pcm.dropProgress = int(48*tc.inflight/time.Millisecond), true, tc.noRewind, tc.progress
			p := &twistPCM{fakePCM: pcm, dropSuspend: tc.dropSuspend}
			gate := make(chan struct{})
			out := NewOutput(OutputConfig{IdleClose: cmp.Or(tc.idle, 100*time.Millisecond), Open: func(string) (PlaybackStream, int, error) {
				<-gate
				return newALSAStream(p, "fake", alsaParams{rate: pcm.rate, channels: 1, period: pcm.rate / 100, buffer: pcm.buffer}), pcm.rate, nil
			}})
			t.Cleanup(func() { _ = out.Close() })
			cancelled := make(chan struct{})
			cancel := func() {
				pcm.mu.Lock()
				pcm.delayFails, pcm.statusFails = tc.delayFails, tc.statusFails
				pcm.mu.Unlock()
				out.Cancel("stopped")
				close(cancelled)
			}
			if tc.prepared {
				p.afterFirst = cancel
			} else {
				pcm.fireAt, pcm.fire = 1000, cancel
			}
			ctx := testContext(t)
			kept := out.Begin("kept")
			if err := kept.WriteAll(ctx, rampPCM(max(tc.frames, 4800)), mono48k); err != nil {
				t.Fatal(err)
			}
			if err := out.Begin("stopped").WriteAll(ctx, constant(48000, 200*time.Millisecond, stoppedLevel), mono48k); err != nil {
				t.Fatal(err)
			}
			close(gate)
			probeWait(t, cancelled, "the Cancel")
			err := kept.Drain(ctx)
			if tc.dropSuspend && tc.statusFails {
				if err == nil {
					t.Fatal("the kept input drained with no error, though the loss of its audio could not be counted")
				}
				return
			}
			if err != nil {
				t.Fatalf("the kept input's Drain: %v", err)
			}
			waitFor(t, "the idle stream to close", pcm.isClosed)
			wantRamp(t, rampHeard(pcm), 4800)
		})
	}
}

// TestASuspendTheSpeakerCannotResumeFromLosesNoAudio fails if a suspend of
// the PCM, as of the system, loses, repeats or reorders audio: a PCM that
// cannot resume is prepared, which discards what it holds, and the stream
// went on as after a lossless recovery (Sol's review of #133 at 4b7f919: the
// room heard 1,440 of a speech's 4,800 frames). What it held, and what was
// in flight to a USB speaker, which the suspend cancels and the status no
// longer counts (Sol's review of #148 at c96723a: 144 frames lost), plays
// again from the Output's history, also through writes the PCM takes in part
// and a second suspend while it replays. What is in flight comes from one
// status snapshot: a pause between a delay query and an avail query must not
// count heard audio as in flight (Sol's review of #148 at fd1d2b6: 960 frames
// repeated). An input begun during the suspend, which played nothing yet,
// must not stop it (Sol's review of #148 at c96723a: a division by zero).
// When the status, or the last in-flight count, could not be read, the loss
// is unknown, and the stream fails, so the speech's Drain reports an error,
// rather than play on with audio gone (Sol's review of #148 at fd1d2b6: 144
// frames lost). A suspend after a write the PCM took whole, before the
// stream reads what is in flight, leaves the last count, which the suspended
// status no longer gives (Sol's review of #148 at 9b80022: 1,440 frames
// lost). The controls resume, and lose nothing.
func TestASuspendTheSpeakerCannotResumeFromLosesNoAudio(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		resumes, statusFails bool
		inflightFails        bool // the status fails until the suspend
		inflight             int
		begin                bool
		partial              int
		resuspend            bool
		split                int
		frames               int  // the speech's frames: 4800 if 0
		afterWrite           bool // suspended after a whole write, before its status
	}{
		{name: "it resumes", resumes: true},
		{name: "it resumes, suspended after a whole write", resumes: true, inflight: 1440, afterWrite: true},
		{name: "it cannot resume"},
		{name: "it cannot resume, 144 frames in flight", inflight: 144},
		{name: "it cannot resume, 144 frames in flight, 960 played between two queries", inflight: 144, split: 960},
		{name: "it cannot resume, 1440 frames in flight, suspended after a whole write", inflight: 1440, afterWrite: true},
		{name: "it cannot resume, an input begun during the suspend", begin: true},
		{name: "it cannot resume, writes of 137 frames", partial: 137},
		{name: "it cannot resume, and is suspended again while the Output replays", resuspend: true},
		// A 200 ms speech, the most the input queues: the mixer has taken
		// 5280 of its frames at the suspend, so the speech still plays.
		{name: "it cannot resume, its status fails", statusFails: true, frames: 9600},
		{name: "it cannot resume, 144 frames in flight, not counted", inflight: 144, inflightFails: true, frames: 9600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pcm := newFakePCM(48000, 48000*int(alsaBuffer/time.Millisecond)/1000)
			pcm.lockstep, pcm.resumes, pcm.inflight, pcm.statusFails = true, tc.resumes, tc.inflight, tc.inflightFails
			p := &twistPCM{fakePCM: pcm, partial: tc.partial, resuspend: tc.resuspend, split: tc.split}
			gate := make(chan struct{})
			out := NewOutput(OutputConfig{IdleClose: 100 * time.Millisecond, Open: func(string) (PlaybackStream, int, error) {
				<-gate
				return newALSAStream(p, "fake", alsaParams{rate: pcm.rate, channels: 1, period: pcm.rate / 100, buffer: pcm.buffer}), pcm.rate, nil
			}})
			t.Cleanup(func() { _ = out.Close() })
			// The speech is frames 0, 1, ...; at the device's frame 1440 it
			// holds the next 3840.
			if tc.afterWrite {
				p.statusAt = 1440
			} else {
				pcm.fireAt, pcm.fire = 1440, func() {
					pcm.mu.Lock()
					pcm.statusFails = tc.statusFails
					pcm.mu.Unlock()
					pcm.suspend()
					if tc.begin {
						out.Begin("late")
					}
				}
			}
			ctx := testContext(t)
			pb := out.Begin(InputVoice)
			frames := max(tc.frames, 4800)
			if err := pb.WriteAll(ctx, rampPCM(frames), mono48k); err != nil {
				t.Fatal(err)
			}
			close(gate)
			err := pb.Drain(ctx)
			if tc.statusFails || tc.inflightFails {
				if err == nil {
					t.Fatal("the speech drained with no error, though the loss of its audio could not be counted")
				}
				return
			}
			if err != nil {
				t.Fatalf("the speech's Drain: %v", err)
			}
			waitFor(t, "the idle stream to close", pcm.isClosed)
			if _, fired := pcm.cancelClock(); !fired {
				t.Fatal("control: the device never reached the suspend")
			}
			wantRamp(t, rampHeard(pcm), frames)
		})
	}
}
