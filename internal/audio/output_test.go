package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

// playedFrame is one frame a fakeStream played, with the time its Write began.
type playedFrame struct {
	at      time.Time
	samples []int16
}

// fakeStream is a playback stream that plays like a device whose buffer is
// full: each Write takes as long as its frame lasts. It keeps every frame it
// played.
type fakeStream struct {
	rate int
	// block, when set, makes every Write wait until it closes.
	block chan struct{}
	// writing is closed at the first Write.
	writing     chan struct{}
	writingOnce sync.Once
	// closePanics makes Close panic, as a PortAudio close that fails does;
	// closeErr makes it return that error.
	closePanics bool
	closeErr    error

	mu       sync.Mutex
	frames   []playedFrame
	closed   bool
	writeErr error
}

func newFakeStream(rate int) *fakeStream {
	return &fakeStream{rate: rate, writing: make(chan struct{})}
}

func (f *fakeStream) Write(frame []int16) error {
	at := time.Now()
	f.mu.Lock()
	f.frames = append(f.frames, playedFrame{at: at, samples: slices.Clone(frame)})
	err := f.writeErr
	f.mu.Unlock()
	f.writingOnce.Do(func() { close(f.writing) })
	if f.block != nil {
		<-f.block
	}
	time.Sleep(time.Duration(len(frame)) * time.Second / time.Duration(f.rate))
	return err
}

func (f *fakeStream) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	if f.closePanics {
		panic("output stream close failed")
	}
	return f.closeErr
}

// Drop has nothing to drop: the fake holds no frames once its Write returns.
func (f *fakeStream) Drop() (int, error) { return 0, nil }

// waitWriting fails the test unless the stream is written within five
// seconds.
func (f *fakeStream) waitWriting(t *testing.T) {
	t.Helper()
	select {
	case <-f.writing:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was never written")
	}
}

func (f *fakeStream) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeStream) played() []playedFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.frames)
}

// samples is every sample the stream played, in order.
func (f *fakeStream) samples() []int16 {
	var all []int16
	for _, frame := range f.played() {
		all = append(all, frame.samples...)
	}
	return all
}

// fakeOpener hands out its streams in turn as an OutputConfig.Open, and the
// last one again once they run out. With gate set, each open waits for it to
// close; with err set, each open fails.
type fakeOpener struct {
	streams []*fakeStream
	gate    chan struct{}
	err     error

	mu      sync.Mutex
	devices []string
}

func (f *fakeOpener) open(device string) (PlaybackStream, int, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices = append(f.devices, device)
	if f.err != nil {
		return nil, 0, f.err
	}
	s := f.streams[min(len(f.devices), len(f.streams))-1]
	return s, s.rate, nil
}

func (f *fakeOpener) opened() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.devices)
}

// newTestOutput makes an Output that opens through opener, closed when the
// test ends.
func newTestOutput(t *testing.T, cfg OutputConfig, opener *fakeOpener) *Output {
	t.Helper()
	cfg.Open = opener.open
	out := NewOutput(cfg)
	t.Cleanup(func() { _ = out.Close() })
	return out
}

var (
	mono48k   = AudioFormat{SampleRate: 48000, Channels: 1}
	stereo48k = AudioFormat{SampleRate: 48000, Channels: 2}
)

// pcm16Bytes is samples as PCM16 little-endian bytes.
func pcm16Bytes(samples []int16) []byte {
	buf := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(s))
	}
	return buf
}

// constant is d of interleaved frames whose channels hold values, at rate.
func constant(rate int, d time.Duration, values ...int16) []byte {
	frames := int(int64(rate) * int64(d) / int64(time.Second))
	samples := make([]int16, 0, frames*len(values))
	for range frames {
		samples = append(samples, values...)
	}
	return pcm16Bytes(samples)
}

// sine is d of a mono tone of freq Hz and amplitude amp, at rate.
func sine(rate int, d time.Duration, freq, amp float64) []byte {
	frames := int(int64(rate) * int64(d) / int64(time.Second))
	samples := make([]int16, frames)
	for n := range samples {
		samples[n] = int16(math.Round(amp * math.Sin(2*math.Pi*freq*float64(n)/float64(rate))))
	}
	return pcm16Bytes(samples)
}

func silent(samples []int16) bool {
	for _, s := range samples {
		if s != 0 {
			return false
		}
	}
	return true
}

// waitFor fails the test if cond does not hold within five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestOutputCancelSilencesTheNextFrameWithin20ms fails if the mixer hands the
// stream frames of the voice for more than 20 ms after Cancel: the time from
// Cancel to the first silent frame, counted in the stream's time, on a stream
// that plays in real time and holds nothing (what a device holds is the ALSA
// path's: TestALSAOutputCancelSilencesTheVoiceWithin20ms). The stream's time
// is the frame under way when Cancel comes and each frame of the voice the
// mixer hands it after: at most one, which it mixed before Cancel took the
// lock. The wall-clock time, logged, adds the machine's scheduling. The voice
// must then stay silent, and its writer must learn of the cancel.
func TestOutputCancelSilencesTheNextFrameWithin20ms(t *testing.T) {
	stream := newFakeStream(48000)
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}})
	ctx := testContext(t)
	var longest time.Duration
	for trial := range 5 {
		pb := out.Begin(InputVoice, nil)
		begun := time.Now()
		written := make(chan error, 1)
		go func() { written <- pb.WriteAll(ctx, constant(48000, 2*time.Second, 1000), mono48k) }()
		waitFor(t, "the voice to play for 50 ms", func() bool {
			audible := 0
			for _, f := range stream.played() {
				if f.at.After(begun) && !silent(f.samples) {
					audible += len(f.samples)
				}
			}
			return audible >= 48000/20
		})
		cancelled := time.Now()
		out.Cancel(InputVoice)
		var silentAt time.Time
		voiceAfter := 0
		waitFor(t, "a silent frame after the cancel", func() bool {
			voiceAfter = 0
			for _, f := range stream.played() {
				if !f.at.After(cancelled) {
					continue
				}
				if silent(f.samples) {
					silentAt = f.at
					return true
				}
				voiceAfter++
			}
			return false
		})
		if streamTime := time.Duration(voiceAfter+1) * outputFrame; streamTime > 20*time.Millisecond {
			t.Fatalf("trial %d: the stream played the voice for %v after Cancel (%d frames of it after the cancel), want silence within 20 ms", trial, streamTime, voiceAfter)
		}
		longest = max(longest, silentAt.Sub(cancelled))
		if err := <-written; !errors.Is(err, ErrCancelled) {
			t.Fatalf("trial %d: WriteAll after Cancel = %v, want ErrCancelled", trial, err)
		}
		time.Sleep(30 * time.Millisecond)
		for _, f := range stream.played() {
			if !f.at.Before(silentAt) && !silent(f.samples) {
				t.Fatalf("trial %d: the voice played again %v after it went silent", trial, f.at.Sub(silentAt))
			}
		}
	}
	t.Logf("longest wall-clock time from Cancel to the first silent frame: %v", longest)
}

// TestOutputMixesTwoInputs fails if two inputs that do not duck each other,
// the chime and an input of another name, do not sum into one stream, sample
// by sample, with a stereo input taken as the mean of its channels and a sum
// beyond PCM16 clipped rather than wrapped.
func TestOutputMixesTwoInputs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		voice, chime []byte
		voiceFormat  AudioFormat
		chimeFormat  AudioFormat
		want         int16
	}{
		{name: "sum", voice: constant(48000, 100*time.Millisecond, 1000), voiceFormat: mono48k,
			chime: constant(48000, 100*time.Millisecond, 1500, 2500), chimeFormat: stereo48k, want: 3000},
		{name: "clipped high", voice: constant(48000, 100*time.Millisecond, 30000), voiceFormat: mono48k,
			chime: constant(48000, 100*time.Millisecond, 30000), chimeFormat: mono48k, want: math.MaxInt16},
		{name: "clipped low", voice: constant(48000, 100*time.Millisecond, -30000), voiceFormat: mono48k,
			chime: constant(48000, 100*time.Millisecond, -30000), chimeFormat: mono48k, want: math.MinInt16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := make(chan struct{})
			stream := newFakeStream(48000)
			out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}, gate: gate})
			ctx := testContext(t)
			voice, chime := out.Begin("other", nil), out.Begin(InputChime, nil)
			if err := voice.WriteAll(ctx, tc.voice, tc.voiceFormat); err != nil {
				t.Fatal(err)
			}
			if err := chime.WriteAll(ctx, tc.chime, tc.chimeFormat); err != nil {
				t.Fatal(err)
			}
			close(gate) // both are queued before the stream opens
			if err := voice.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			if err := chime.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			mixed, other := 0, map[int16]int{}
			for _, s := range stream.samples() {
				switch s {
				case tc.want:
					mixed++
				case 0:
				default:
					other[s]++
				}
			}
			if mixed != 4800 || len(other) > 0 {
				t.Fatalf("the stream played %d samples of %d (want 4800, 100 ms at 48 kHz) and other values %v", mixed, tc.want, other)
			}
		})
	}
}

// TestOutputDucksTheVoiceUnderASound fails if a sound on the chime input does
// not duck the voice playing under it to duckGain, or the sound overlaps the
// voice before it is down, or the voice's gain jumps rather than moving over
// about 10 ms, down and back up, or the sound does not play whole at its own
// level. A voice that starts under a sound starts ducked, and a sound over a
// silent voice does not wait.
func TestOutputDucksTheVoiceUnderASound(t *testing.T) {
	const voiceLevel, soundLevel = 16000, 1000
	ducked := int16(math.Round(voiceLevel * float64(duckGain)))
	t.Run("over a voice already playing", func(t *testing.T) {
		stream := newFakeStream(48000)
		out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}})
		ctx := testContext(t)
		voice, chime := out.Begin(InputVoice, nil), out.Begin(InputChime, nil)
		spoken := make(chan error, 1)
		go func() {
			err := voice.WriteAll(ctx, constant(48000, 600*time.Millisecond, voiceLevel), mono48k)
			if err == nil {
				err = voice.Drain(ctx)
			}
			spoken <- err
		}()
		waitFor(t, "50 ms of the voice", func() bool { return len(stream.played()) >= 5 })
		if err := chime.WriteAll(ctx, constant(48000, 50*time.Millisecond, soundLevel), mono48k); err != nil {
			t.Fatal(err)
		}
		if err := <-spoken; err != nil {
			t.Fatal(err)
		}
		got := stream.samples()
		under := slices.Index(got, ducked+soundLevel)
		if under < 0 {
			t.Fatalf("the sound never played over the voice at %d (%d ducked and the sound's %d)", ducked+soundLevel, ducked, soundLevel)
		}
		n := 0
		for under+n < len(got) && got[under+n] == ducked+soundLevel {
			n++
		}
		if n != 2400 {
			t.Errorf("the sound played over the ducked voice for %d samples, want all of its 2400 (50 ms at 48 kHz)", n)
		}
		if got[under-1] != ducked {
			t.Errorf("just before the sound the voice played at %d, want %d: the voice is down before the sound starts", got[under-1], ducked)
		}
		if down := slices.IndexFunc(got, func(s int16) bool { return s != voiceLevel }); down < 0 || under-down > 480 {
			t.Errorf("the voice began going down at sample %d and the sound started at %d, want the sound within 480 samples (10 ms): it waits only for the voice to go down", down, under)
		}
		// The voice's part of the stream moves at most this much from one
		// sample to the next while it plays: its gain goes all the way down or
		// up over no less than 9 ms.
		voiceAt := func(i int) int {
			if i >= under && i < under+n {
				return int(got[i]) - soundLevel
			}
			return int(got[i])
		}
		step := (voiceLevel - int(ducked)) / 432
		for i := 1; i < len(got) && got[i] != 0; i++ {
			if d := voiceAt(i) - voiceAt(i-1); d > step || d < -step {
				t.Fatalf("the voice jumps from %d to %d at sample %d, more than the %d a 9 ms change of its gain allows: a click", voiceAt(i-1), voiceAt(i), i, step)
			}
		}
		if back := slices.Index(got[under+n:], voiceLevel); back < 0 || back > 480 {
			t.Errorf("the voice came back to %d %d samples after the sound, want within 480 (10 ms)", voiceLevel, back)
		}
	})
	t.Run("a voice that starts under it, and over silence", func(t *testing.T) {
		gate := make(chan struct{})
		stream := newFakeStream(48000)
		out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}, gate: gate})
		ctx := testContext(t)
		voice, chime := out.Begin(InputVoice, nil), out.Begin(InputChime, nil)
		if err := chime.WriteAll(ctx, constant(48000, 50*time.Millisecond, soundLevel), mono48k); err != nil {
			t.Fatal(err)
		}
		if err := voice.WriteAll(ctx, constant(48000, 100*time.Millisecond, voiceLevel), mono48k); err != nil {
			t.Fatal(err)
		}
		close(gate) // both are queued before the stream opens
		if err := voice.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		got := stream.samples()
		if len(got) < 4800 || slices.ContainsFunc(got[:2400], func(s int16) bool { return s != ducked+soundLevel }) || got[4799] != voiceLevel {
			t.Errorf("the stream began %v, want the sound at once over the voice %d down, all 2400 samples, then the voice back at %d", got[:min(len(got), 8)], ducked, voiceLevel)
		}
	})
}

// TestOutputResamplesToTheStreamRate fails if an input at another rate than
// the stream's is not converted cleanly: a tone written in 20 ms frames must
// come out as that tone, at its amplitude, for its duration, with less than
// -50 dB of everything else. Linear interpolation from 24 kHz to 48 kHz leaves
// the image of a 6 kHz tone at 18 kHz only 21 dB down, and a resampler that
// starts each frame afresh adds a click at every frame boundary.
func TestOutputResamplesToTheStreamRate(t *testing.T) {
	for _, tc := range []struct{ from, to int }{
		{from: 24000, to: 48000},
		{from: 44100, to: 48000},
		{from: 16000, to: 48000},
		{from: 48000, to: 16000},
	} {
		t.Run(fmt.Sprintf("%d to %d", tc.from, tc.to), func(t *testing.T) {
			const amp = 8000.0
			tone := 6000.0
			if tc.to < tc.from {
				tone = 1000
			}
			stream := newFakeStream(tc.to)
			out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}})
			ctx := testContext(t)
			pb := out.Begin(InputVoice, nil)
			if err := pb.WriteAll(ctx, sine(tc.from, 500*time.Millisecond, tone, amp), AudioFormat{SampleRate: tc.from, Channels: 1}); err != nil {
				t.Fatal(err)
			}
			if err := pb.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			got := stream.samples()
			first := slices.IndexFunc(got, func(s int16) bool { return s != 0 })
			if first < 0 {
				t.Fatal("the stream played only silence")
			}
			last := len(got) - 1
			for got[last] == 0 {
				last--
			}
			// The filter rings for half its taps at each end of the tone.
			var rs resampler
			rs.configure(tc.from, tc.to)
			ringing := rs.taps * tc.to / tc.from
			if span, want := last-first+1, tc.to/2; span < want-tc.to/1000 || span > want+tc.to/1000+ringing {
				t.Fatalf("the tone lasted %d samples at %d Hz, want %d (500 ms) and at most %d of filter ringing", span, tc.to, want, ringing)
			}
			edge := tc.to / 100 // leave out 10 ms at each end, where the filter settles
			amplitude, snr := fitTone(got[first+edge:last-edge], tone, float64(tc.to))
			if gain := 20 * math.Log10(amplitude/amp); math.Abs(gain) > 0.5 {
				t.Fatalf("the tone came out at amplitude %.0f (%.2f dB), want %.0f", amplitude, gain, amp)
			}
			if snr < 50 {
				t.Fatalf("the tone came out with %.1f dB of signal over everything else, want at least 50 dB", snr)
			}
		})
	}
}

// fitTone fits a sine of freq Hz to samples at rate by least squares and
// returns its amplitude and the ratio, in dB, of its power to the residual's.
func fitTone(samples []int16, freq, rate float64) (amplitude, snr float64) {
	w := 2 * math.Pi * freq / rate
	var ss, cc, sc, ys, yc float64
	for n, y := range samples {
		s, c := math.Sin(w*float64(n)), math.Cos(w*float64(n))
		ss, cc, sc = ss+s*s, cc+c*c, sc+s*c
		ys, yc = ys+float64(y)*s, yc+float64(y)*c
	}
	det := ss*cc - sc*sc
	a, b := (ys*cc-yc*sc)/det, (yc*ss-ys*sc)/det
	var signal, residual float64
	for n, y := range samples {
		fit := a*math.Sin(w*float64(n)) + b*math.Cos(w*float64(n))
		signal += fit * fit
		residual += (float64(y) - fit) * (float64(y) - fit)
	}
	return math.Hypot(a, b), 10 * math.Log10(signal/residual)
}

// TestOutputHoldsAtMost200msPerInput fails if an input queues more than 200 ms
// of audio: a writer faster than real time must wait, and go on once the
// stream has played some. Another input is not held back by a full one.
func TestOutputHoldsAtMost200msPerInput(t *testing.T) {
	gate := make(chan struct{})
	stream := newFakeStream(48000)
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}, gate: gate})
	ctx := testContext(t)
	voice := out.Begin(InputVoice, nil)
	frame := constant(48000, MaxWriteFrame, 1000)
	for i := range 10 {
		if err := voice.Write(ctx, frame, mono48k); err != nil {
			t.Fatalf("write %d of the first 200 ms: %v", i+1, err)
		}
	}
	held, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := voice.Write(held, frame, mono48k); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an eleventh 20 ms frame while nothing played = %v, want it to wait (more than 200 ms queued)", err)
	}
	chime := out.Begin(InputChime, nil)
	if err := chime.WriteAll(ctx, constant(48000, InputQueue, 500), mono48k); err != nil {
		t.Fatalf("the chime with the voice's queue full: %v", err)
	}
	close(gate)
	if err := voice.Write(ctx, frame, mono48k); err != nil {
		t.Fatalf("a write once the stream plays: %v", err)
	}
}

// TestOutputReportsSpeechStartedAndEnded fails if an input's speech does not
// start and end as events, which barge-in needs: Started when its first audio
// reaches the stream, Ended once it has all played, with how much played, or
// at once on Cancel, marked cancelled.
func TestOutputReportsSpeechStartedAndEnded(t *testing.T) {
	stream := newFakeStream(48000)
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}})
	events := make(chan SpeechEvent, 16)
	remove := out.OnSpeech(func(e SpeechEvent) { events <- e })
	defer remove()
	next := func(what string) SpeechEvent {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(2 * time.Second):
			t.Fatalf("no event for %s", what)
			return SpeechEvent{}
		}
	}
	ctx := testContext(t)

	began := time.Now()
	pb := out.Begin(InputChime, nil)
	if err := pb.WriteAll(ctx, constant(48000, 100*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	started := next("the chime's start")
	if started.Input != InputChime || !started.Started || started.At.Before(began) {
		t.Fatalf("first event = %+v, want the chime started", started)
	}
	ended := next("the chime's end")
	if ended.Input != InputChime || ended.Started || ended.Cancelled || ended.At.Before(started.At) {
		t.Fatalf("second event = %+v, want the chime ended after it started, not cancelled", ended)
	}
	if ended.Played < 100*time.Millisecond || ended.Played > 110*time.Millisecond {
		t.Fatalf("the chime's end reports %v played, want 100 ms", ended.Played)
	}

	pb = out.Begin(InputVoice, nil)
	go func() { _ = pb.WriteAll(ctx, constant(48000, 2*time.Second, 1000), mono48k) }()
	if e := next("the voice's start"); e.Input != InputVoice || !e.Started {
		t.Fatalf("event = %+v, want the voice started", e)
	}
	time.Sleep(50 * time.Millisecond)
	out.Cancel(InputVoice)
	e := next("the voice's end")
	if e.Input != InputVoice || e.Started || !e.Cancelled || e.Played <= 0 || e.Played >= time.Second {
		t.Fatalf("event after Cancel = %+v, want the voice ended, cancelled, with the part that played", e)
	}
	if played := pb.Played(); played != e.Played {
		t.Fatalf("the playback reports %v played, its end event %v", played, e.Played)
	}
}

// TestASpeechEventCarriesItsStartingPlaybacksOwner fails if a speech's start
// and end events do not carry the owner of the playback whose audio started
// it (Begin's owner), so a listener can tell whose speech an event is
// however late it comes: on an end of its own, on a Cancel, and for a speech
// whose audio a second playback continues, still the first's. A playback
// begun with no owner carries none. The owner is for the voice's exchanges
// (#143).
func TestASpeechEventCarriesItsStartingPlaybacksOwner(t *testing.T) {
	stream := newFakeStream(48000)
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}})
	events := make(chan SpeechEvent, 16)
	remove := out.OnSpeech(func(e SpeechEvent) { events <- e })
	defer remove()
	next := func(what string) SpeechEvent {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(2 * time.Second):
			t.Fatalf("no event for %s", what)
			return SpeechEvent{}
		}
	}
	ctx := testContext(t)
	type owner struct{ name string }
	first, second := &owner{"first"}, &owner{"second"}

	// Two playbacks in one speech: its end is still the first's.
	a := out.Begin(InputVoice, first)
	if err := a.WriteAll(ctx, constant(48000, 40*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	b := out.Begin(InputVoice, second)
	if err := b.WriteAll(ctx, constant(48000, 40*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	if e := next("the first speech's start"); !e.Started || e.Owner != first {
		t.Fatalf("start = %+v, want the first playback's owner", e)
	}
	if e := next("the first speech's end"); e.Started || e.Cancelled || e.Owner != first {
		t.Fatalf("end = %+v, want the first playback's owner, not cancelled", e)
	}

	// A speech the second playback starts is its own; a Cancel's end too.
	c := out.Begin(InputVoice, second)
	go func() { _ = c.WriteAll(ctx, constant(48000, 2*time.Second, 1000), mono48k) }()
	if e := next("the second speech's start"); !e.Started || e.Owner != second {
		t.Fatalf("start = %+v, want the second playback's owner", e)
	}
	time.Sleep(50 * time.Millisecond)
	out.Cancel(InputVoice)
	if e := next("the second speech's cut"); e.Started || !e.Cancelled || e.Owner != second {
		t.Fatalf("end after Cancel = %+v, want the second playback's owner, cancelled", e)
	}

	// No owner: none on either event.
	d := out.Begin(InputVoice, nil)
	if err := d.WriteAll(ctx, constant(48000, 40*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	if e := next("the ownerless speech's start"); !e.Started || e.Owner != nil {
		t.Fatalf("start = %+v, want no owner", e)
	}
	if e := next("the ownerless speech's end"); e.Started || e.Owner != nil {
		t.Fatalf("end = %+v, want no owner", e)
	}
}

// heldAudioStream is a fakeStream whose first Write with audio in it waits
// first, as a device's first write can, and records when it returned: for
// hold, or, with release set, until release closes, having said on entered
// that it waits.
type heldAudioStream struct {
	*fakeStream
	hold     time.Duration
	entered  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	returned time.Time
}

func (s *heldAudioStream) Write(frame []int16) error {
	s.mu.Lock()
	first := s.returned.IsZero() && slices.ContainsFunc(frame, func(v int16) bool { return v != 0 })
	s.mu.Unlock()
	if first {
		if s.release != nil {
			s.entered <- struct{}{}
			<-s.release
		} else {
			time.Sleep(s.hold)
		}
	}
	err := s.fakeStream.Write(frame)
	if first {
		s.mu.Lock()
		s.returned = time.Now()
		s.mu.Unlock()
	}
	return err
}

// TestASpeechStartsWhenTheStreamTakesItsFirstAudio fails if an input's speech
// is reported started before the stream took its first audio, as when the
// device holds the first write back: the voice's barge-in counts words said
// after that moment as talking over it, and words said while the write waited
// talked over nothing the room could hear.
func TestASpeechStartsWhenTheStreamTakesItsFirstAudio(t *testing.T) {
	stream := &heldAudioStream{fakeStream: newFakeStream(48000), hold: 300 * time.Millisecond}
	out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, 48000, nil }})
	t.Cleanup(func() { _ = out.Close() })
	events := make(chan SpeechEvent, 16)
	defer out.OnSpeech(func(e SpeechEvent) { events <- e })()
	pb := out.Begin(InputVoice, nil)
	go func() { _ = pb.WriteAll(testContext(t), constant(48000, time.Second, 1000), mono48k) }()
	select {
	case e := <-events:
		stream.mu.Lock()
		returned := stream.returned
		stream.mu.Unlock()
		if !e.Started || returned.IsZero() || e.At.Before(returned) {
			t.Fatalf("the voice started at %v, %v before the stream took its first audio at %v", e.At.Format(time.StampMicro), returned.Sub(e.At), returned.Format(time.StampMicro))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no speech event")
	}
}

// TestASpeechCutWhileItsFirstWriteIsHeldNeverStarts fails if a speech that
// Cancel cut while the device held its first write reports a start and an
// end once the write returns: they came after whatever took the speaker over
// and were taken for its speech (Sol's review of #133 at 55bdf53). The
// control, the same speech with no cut, starts.
func TestASpeechCutWhileItsFirstWriteIsHeldNeverStarts(t *testing.T) {
	for _, cut := range []bool{true, false} {
		t.Run(fmt.Sprintf("cut %v", cut), func(t *testing.T) {
			stream := &heldAudioStream{fakeStream: newFakeStream(48000), entered: make(chan struct{}, 1), release: make(chan struct{})}
			out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, 48000, nil }})
			t.Cleanup(func() { _ = out.Close() })
			events := make(chan SpeechEvent, 16)
			defer out.OnSpeech(func(e SpeechEvent) { events <- e })()
			pb := out.Begin(InputVoice, nil)
			go func() { _ = pb.WriteAll(testContext(t), constant(48000, time.Second, 1000), mono48k) }()
			<-stream.entered // the first write waits on the device
			if cut {
				out.Cancel(InputVoice)
			}
			close(stream.release)
			select {
			case e := <-events:
				if cut {
					t.Fatalf("the cut speech reported %+v", e)
				}
			case <-time.After(time.Second):
				if !cut {
					t.Fatal("control: the speech never started")
				}
			}
		})
	}
}

// lockedAfterWriteStream is a fakeStream that records when its first write
// with audio in it returned, and, with hold set, has another goroutine take
// the Output's lock just before that write returns and keep it for hold.
type lockedAfterWriteStream struct {
	*fakeStream
	out      *Output
	hold     time.Duration
	once     sync.Once
	returned time.Time // the mixer writes it before the speech's start is emitted
}

func (s *lockedAfterWriteStream) Write(frame []int16) error {
	err := s.fakeStream.Write(frame)
	if slices.ContainsFunc(frame, func(v int16) bool { return v != 0 }) {
		s.once.Do(func() {
			if s.hold > 0 {
				locked := make(chan struct{})
				go func() {
					s.out.mu.Lock()
					close(locked)
					time.Sleep(s.hold)
					s.out.mu.Unlock()
				}()
				<-locked
			}
			s.returned = time.Now()
		})
	}
	return err
}

// TestASpeechStartsWhenTheWriteReturnsNotWhenTheLockComes fails if a
// speech's start is stamped when the mixer took the Output's lock after the
// stream accepted its first audio, not when the write returned: a wait for
// the lock moved the start later, and words said over the voice in that time
// fell outside its speech (Sol's review of #133 at b41b12b found 150 ms). The
// control has no wait for the lock.
func TestASpeechStartsWhenTheWriteReturnsNotWhenTheLockComes(t *testing.T) {
	for _, hold := range []time.Duration{0, 150 * time.Millisecond} {
		t.Run(fmt.Sprintf("the lock held %v", hold), func(t *testing.T) {
			stream := &lockedAfterWriteStream{fakeStream: newFakeStream(48000), hold: hold}
			out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, 48000, nil }})
			stream.out = out
			t.Cleanup(func() { _ = out.Close() })
			events := make(chan SpeechEvent, 16)
			defer out.OnSpeech(func(e SpeechEvent) { events <- e })()
			go func() {
				_ = out.Begin(InputVoice, nil).WriteAll(testContext(t), constant(48000, 100*time.Millisecond, 1000), mono48k)
			}()
			select {
			case e := <-events:
				if !e.Started {
					t.Fatalf("control: the first event is %+v, want the start", e)
				}
				if late := e.At.Sub(stream.returned); late < 0 || late > 50*time.Millisecond {
					t.Fatalf("the speech started %v after its first write returned, want at it", late.Round(time.Millisecond))
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the speech never started")
			}
		})
	}
}

// ordinaryEndStream is a playback stream that plays in real time, records
// when its last write with audio in it returned, and holds its first silent
// write until release closes, having closed held.
type ordinaryEndStream struct {
	mu                     sync.Mutex
	lastVoice, firstSilent time.Time
	held, release          chan struct{}
	holdOnce               sync.Once
}

func (s *ordinaryEndStream) Write(frame []int16) error {
	voiced := slices.ContainsFunc(frame, func(v int16) bool { return v != 0 })
	if !voiced {
		s.holdOnce.Do(func() {
			s.mu.Lock()
			s.firstSilent = time.Now()
			s.mu.Unlock()
			close(s.held)
			<-s.release
		})
	}
	time.Sleep(time.Duration(len(frame)) * time.Second / 48000)
	if voiced {
		s.mu.Lock()
		s.lastVoice = time.Now()
		s.mu.Unlock()
	}
	return nil
}

func (*ordinaryEndStream) Drop() (int, error) { return 0, nil }
func (*ordinaryEndStream) Close() error       { return nil }

// TestASpeechEndsNoLaterThanTheStreamTookItsLastAudio fails if a speech that
// ends by itself is stamped when its end was found rather than when the
// stream took its last audio: the device held the silent write after it, so
// the end was found late, and silence the room heard fell inside the speech.
// TestASpeechEndsWhenTheStreamTakesItsLastAudio bounds the end from below
// only (Sol's review of #133 at ab139c8).
func TestASpeechEndsNoLaterThanTheStreamTookItsLastAudio(t *testing.T) {
	s := &ordinaryEndStream{held: make(chan struct{}), release: make(chan struct{})}
	gate := make(chan struct{})
	out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) {
		<-gate
		return s, 48000, nil
	}})
	open, release := sync.OnceFunc(func() { close(gate) }), sync.OnceFunc(func() { close(s.release) })
	t.Cleanup(func() {
		release()
		open()
		if err := out.Close(); err != nil {
			t.Errorf("close the Output: %v", err)
		}
	})
	events := make(chan SpeechEvent, 4)
	defer out.OnSpeech(func(e SpeechEvent) { events <- e })()
	if err := out.Begin(InputVoice, nil).WriteAll(testContext(t), constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	open()
	select {
	case <-s.held:
	case <-time.After(3 * time.Second):
		t.Fatal("control: the first silent write was not held")
	}
	time.Sleep(150 * time.Millisecond) // the end is found only after this
	release()
	var got []SpeechEvent
	for len(got) < 2 {
		select {
		case e := <-events:
			got = append(got, e)
		case <-time.After(3 * time.Second):
			t.Fatalf("speech events %+v, want a start and an end", got)
		}
	}
	s.mu.Lock()
	lastVoice, firstSilent := s.lastVoice, s.firstSilent
	s.mu.Unlock()
	if lastVoice.IsZero() || !firstSilent.After(lastVoice) || !got[0].Started || got[1].Started || got[1].Cancelled {
		t.Fatalf("control: the last voiced write returned at %v, the first silent one began at %v, events %+v", lastVoice, firstSilent, got)
	}
	if got[1].At.Before(lastVoice) || !got[1].At.Before(firstSilent) {
		t.Fatalf("the speech ended at %v, want from the last voiced write's return at %v to before the first silent write at %v", got[1].At.Format(time.StampMicro), lastVoice.Format(time.StampMicro), firstSilent.Format(time.StampMicro))
	}
}

// TestAShortSpeechEndsNoEarlierThanItStarts fails if a speech whose first
// write the device held reports an end before its start: its last audio was
// mixed before the write returned (Sol's review of #133 at 55bdf53).
func TestAShortSpeechEndsNoEarlierThanItStarts(t *testing.T) {
	stream := &heldAudioStream{fakeStream: newFakeStream(48000), hold: 300 * time.Millisecond}
	out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, 48000, nil }})
	t.Cleanup(func() { _ = out.Close() })
	events := make(chan SpeechEvent, 16)
	defer out.OnSpeech(func(e SpeechEvent) { events <- e })()
	pb := out.Begin(InputVoice, nil)
	go func() { _ = pb.WriteAll(testContext(t), constant(48000, 10*time.Millisecond, 1000), mono48k) }()
	var got []SpeechEvent
	for len(got) < 2 {
		select {
		case e := <-events:
			got = append(got, e)
		case <-time.After(3 * time.Second):
			t.Fatalf("speech events %+v, want a start and an end", got)
		}
	}
	if !got[0].Started || got[1].Started || got[1].At.Before(got[0].At) {
		t.Fatalf("speech events %+v: the end comes %v before the start", got, got[0].At.Sub(got[1].At))
	}
}

// lastWriteStream is a fakeStream whose write of the nth frame with audio
// in it waits until release closes, and records when that write returned.
type lastWriteStream struct {
	*fakeStream
	nth      int
	release  chan struct{}
	mu       sync.Mutex
	audible  int
	returned time.Time
}

func (s *lastWriteStream) Write(frame []int16) error {
	s.mu.Lock()
	if slices.ContainsFunc(frame, func(v int16) bool { return v != 0 }) {
		s.audible++
	}
	held := s.audible == s.nth && s.returned.IsZero()
	s.mu.Unlock()
	if held {
		<-s.release
	}
	err := s.fakeStream.Write(frame)
	if held {
		s.mu.Lock()
		s.returned = time.Now()
		s.mu.Unlock()
	}
	return err
}

// TestASpeechEndsWhenTheStreamTakesItsLastAudio fails if a speech's end is
// stamped before the stream took its last audio, as when the device holds
// that write back: words said over the voice while it played that audio fell
// outside the speech (Sol's review of #133 at 795ad04).
func TestASpeechEndsWhenTheStreamTakesItsLastAudio(t *testing.T) {
	stream := &lastWriteStream{fakeStream: newFakeStream(48000), nth: 3, release: make(chan struct{})}
	out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, 48000, nil }})
	t.Cleanup(func() { _ = out.Close() })
	events := make(chan SpeechEvent, 16)
	defer out.OnSpeech(func(e SpeechEvent) { events <- e })()
	pb := out.Begin(InputVoice, nil)
	go func() { _ = pb.WriteAll(testContext(t), constant(48000, 30*time.Millisecond, 1000), mono48k) }()
	time.AfterFunc(300*time.Millisecond, func() { close(stream.release) }) // the device holds its last write
	for {
		select {
		case e := <-events:
			if e.Started {
				continue
			}
			stream.mu.Lock()
			returned := stream.returned
			stream.mu.Unlock()
			if returned.IsZero() || e.At.Before(returned) {
				t.Fatalf("the speech ended at %v, %v before the stream took its last audio at %v", e.At.Format(time.StampMicro), returned.Sub(e.At), returned.Format(time.StampMicro))
			}
			return
		case <-time.After(3 * time.Second):
			t.Fatal("the speech did not end")
		}
	}
}

// replayStream is a fakeStream that takes back back samples at its first
// Drop, as a device's buffer does, and holds the first write after it, the
// replay's, until release closes, recording when the replay's last write
// with audio in it returned: once the other input is cancelled, the replay's
// audio is the voice's.
type replayStream struct {
	*fakeStream
	back     int
	release  chan struct{}
	mu       sync.Mutex
	dropped  bool
	held     bool
	replayed time.Time
	replay   int // samples of the replay still to write
}

func (s *replayStream) Drop() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return 0, nil
	}
	s.dropped, s.replay = true, s.back
	return s.back, nil
}

func (s *replayStream) Write(frame []int16) error {
	s.mu.Lock()
	inReplay := s.replay > 0
	hold := inReplay && !s.held
	s.held = s.held || hold
	s.mu.Unlock()
	if hold {
		<-s.release
	}
	err := s.fakeStream.Write(frame)
	if inReplay {
		s.mu.Lock()
		s.replay -= len(frame)
		if slices.ContainsFunc(frame, func(v int16) bool { return v != 0 }) {
			s.replayed = time.Now()
		}
		s.mu.Unlock()
	}
	return err
}

// TestASpeechWhoseLastAudioIsReplayedEndsAfterTheReplay fails if a speech
// whose last audio a Cancel of another input took back from the device, and
// the stream then took again, is stamped as ending before that replay's
// write returned (Sol's review of #133 at b6eef5c).
func TestASpeechWhoseLastAudioIsReplayedEndsAfterTheReplay(t *testing.T) {
	const rate = 48000
	stream := &replayStream{fakeStream: newFakeStream(rate), back: rate / 20, release: make(chan struct{})}
	out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, rate, nil }})
	t.Cleanup(func() { _ = out.Close() })
	events := make(chan SpeechEvent, 16)
	defer out.OnSpeech(func(e SpeechEvent) {
		if e.Input == InputVoice {
			events <- e
		}
	})()
	chime := out.Begin(InputChime, nil)
	go func() { _ = chime.WriteAll(testContext(t), constant(rate, time.Second, 7), mono48k) }()
	stream.waitWriting(t)
	voice := out.Begin(InputVoice, nil)
	go func() { _ = voice.WriteAll(testContext(t), constant(rate, 20*time.Millisecond, 1000), mono48k) }()
	// Once the stream has the chime alone after the voice's audio, the
	// voice's last audio is in the device's last 50 ms: cancel the chime.
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		stream.fakeStream.mu.Lock()
		voiced, after := false, false
		for _, f := range stream.frames {
			loudest := slices.Max(f.samples)
			switch {
			case loudest > 100:
				voiced = true
			case voiced && loudest > 0:
				after = true
			}
		}
		stream.fakeStream.mu.Unlock()
		if after {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never had the chime alone after the voice")
		}
	}
	out.Cancel(InputChime)
	time.AfterFunc(200*time.Millisecond, func() { close(stream.release) }) // the replay's write waits on the device
	for {
		select {
		case e := <-events:
			if e.Started {
				continue
			}
			stream.mu.Lock()
			replayed := stream.replayed
			stream.mu.Unlock()
			if replayed.IsZero() || e.At.Before(replayed) {
				t.Fatalf("the voice ended at %v, %v before the replay of its last audio was taken at %v", e.At.Format(time.StampMicro), replayed.Sub(e.At), replayed.Format(time.StampMicro))
			}
			return
		case <-time.After(3 * time.Second):
			t.Fatal("the voice did not end")
		}
	}
}

// boundaryStream takes five ordinary frames, holds the fifth until released
// closes, then takes back those five at its Drop and replays
// them, holding the holdReplay'th replay write until replayRelease closes. It
// records when the replay's last write with audio returned. Each write takes
// as long as its audio plays.
type boundaryStream struct {
	mu                   sync.Mutex
	normalN, replayN     int
	replayLeft           int
	holdReplay           int
	replayVoiceSamples   int
	lastVoiceAccepted    time.Time
	normalHeld, released chan struct{}
	replayHeld, replayGo chan struct{}
}

func (s *boundaryStream) Write(frame []int16) error {
	s.mu.Lock()
	replaying := s.replayLeft > 0
	var hold bool
	if replaying {
		s.replayN++
		hold = s.replayN == s.holdReplay
	} else {
		s.normalN++
		hold = s.normalN == 5
	}
	s.mu.Unlock()
	if hold && replaying {
		close(s.replayHeld)
		<-s.replayGo
	} else if hold {
		close(s.normalHeld)
		<-s.released
	}
	time.Sleep(time.Duration(len(frame)) * time.Second / 48000)
	if replaying {
		s.mu.Lock()
		if slices.ContainsFunc(frame, func(v int16) bool { return v != 0 }) {
			s.lastVoiceAccepted = time.Now()
			s.replayVoiceSamples += len(frame)
		}
		s.replayLeft -= len(frame)
		s.mu.Unlock()
	}
	return nil
}

func (s *boundaryStream) Drop() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replayLeft = 2400 // 50 ms at 48 kHz, the five frames taken
	return s.replayLeft, nil
}

func (*boundaryStream) Close() error { return nil }

// TestAReplayEndsAtTheVoicesLastReplayedAudio fails if a speech whose last
// audio is replayed, followed in the replay by silence, is stamped as ending
// after that silence was taken: silence counted as the voice playing, and
// words said then could count as talk-over (Sol's review of #133 at
// 99f652f). The control holds the replay's last write with voice: the end
// cannot come before it.
func TestAReplayEndsAtTheVoicesLastReplayedAudio(t *testing.T) {
	for _, tc := range []struct {
		name       string
		holdReplay int
		silentTail bool
	}{
		{"control: the last voiced replay write held", 2, false},
		{"a silent replay write after the voice held", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &boundaryStream{
				holdReplay: tc.holdReplay,
				normalHeld: make(chan struct{}), released: make(chan struct{}),
				replayHeld: make(chan struct{}), replayGo: make(chan struct{}),
			}
			openGate := make(chan struct{})
			o := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) {
				<-openGate
				return s, 48000, nil
			}})
			t.Cleanup(func() { _ = o.Close() })
			var openOnce, normalOnce, replayOnce sync.Once
			t.Cleanup(func() { replayOnce.Do(func() { close(s.replayGo) }) })
			t.Cleanup(func() { normalOnce.Do(func() { close(s.released) }) })
			t.Cleanup(func() { openOnce.Do(func() { close(openGate) }) })
			events := make(chan SpeechEvent, 16)
			defer o.OnSpeech(func(e SpeechEvent) {
				if e.Input == InputVoice {
					events <- e
				}
			})()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := o.Begin(InputVoice, nil).WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			if err := o.Begin(InputChime, nil).WriteAll(ctx, constant(48000, 100*time.Millisecond, 7), mono48k); err != nil {
				t.Fatal(err)
			}
			openOnce.Do(func() { close(openGate) })
			select {
			case <-s.normalHeld: // the fifth frame: 20 ms of voice, then chime alone
			case <-time.After(3 * time.Second):
				t.Fatal("never reached the fifth write")
			}
			o.Cancel(InputChime) // the only cancel: the replay holds the voice, then silence
			normalOnce.Do(func() { close(s.released) })
			select {
			case <-s.replayHeld:
			case <-time.After(3 * time.Second):
				t.Fatal("never reached the held replay write")
			}
			heldAt := time.Now()
			replayOnce.Do(func() { close(s.replayGo) })
			var got []SpeechEvent
			for len(got) < 2 {
				select {
				case e := <-events:
					got = append(got, e)
				case <-ctx.Done():
					t.Fatalf("events %+v, want a start and an end", got)
				}
			}
			s.mu.Lock()
			lastVoice, samples := s.lastVoiceAccepted, s.replayVoiceSamples
			s.mu.Unlock()
			if !got[0].Started || got[1].Started || got[1].Cancelled || samples != 960 {
				t.Fatalf("control: events %+v, %d voiced samples replayed, want 960", got, samples)
			}
			if got[1].At.Before(lastVoice) {
				t.Fatalf("the voice ended at %v, before its last replayed audio was taken at %v", got[1].At.Format(time.StampMicro), lastVoice.Format(time.StampMicro))
			}
			if tc.silentTail && !got[1].At.Before(heldAt) {
				t.Fatalf("the voice ended at %v, after the silent replay write began at %v: %v of silence counted as its speech", got[1].At.Format(time.StampMicro), heldAt.Format(time.StampMicro), got[1].At.Sub(lastVoice))
			}
		})
	}
}

// TestOutputOpensTheConfiguredDevice fails if the Output opens any device but
// the speaker_device setting.
func TestOutputOpensTheConfiguredDevice(t *testing.T) {
	opener := &fakeOpener{streams: []*fakeStream{newFakeStream(48000)}}
	out := newTestOutput(t, OutputConfig{Device: "J75"}, opener)
	ctx := testContext(t)
	pb := out.Begin(InputVoice, nil)
	if err := pb.WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	if err := pb.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := opener.opened(); !slices.Equal(got, []string{"J75"}) {
		t.Fatalf("opened %q, want only the configured J75", got)
	}
}

// TestOutputKeepsOneStreamAcrossSpeechesAndClosesItWhenIdle fails if the
// Output opens a stream per speech, or keeps it open once IdleClose has passed
// with nothing to play: the Jabra's light cannot change while a stream is
// open, so a resume waits for the stream to close.
func TestOutputKeepsOneStreamAcrossSpeechesAndClosesItWhenIdle(t *testing.T) {
	first, second := newFakeStream(48000), newFakeStream(48000)
	opener := &fakeOpener{streams: []*fakeStream{first, second}}
	out := newTestOutput(t, OutputConfig{IdleClose: 100 * time.Millisecond}, opener)
	ctx := testContext(t)
	for range 2 {
		pb := out.Begin(InputVoice, nil)
		if err := pb.WriteAll(ctx, constant(48000, 40*time.Millisecond, 1000), mono48k); err != nil {
			t.Fatal(err)
		}
		if err := pb.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(opener.opened()); n != 1 || first.isClosed() {
		t.Fatalf("two speeches 20 ms apart opened %d streams (first closed: %t), want one stream kept open", n, first.isClosed())
	}
	waitFor(t, "the idle stream to close", first.isClosed)
	pb := out.Begin(InputVoice, nil)
	if err := pb.WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	if err := pb.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(opener.opened()); n != 2 || silent(second.samples()) {
		t.Fatalf("after the idle close, a speech opened %d streams in all and played on the new one: %t", n, !silent(second.samples()))
	}
}

// TestOutputHooksBracketTheStream fails if the output hooks do not run around
// the playback stream: opening before the stream opens, and closing once the
// Output is done with it, with the stream still open until closing's
// closeStream runs, which returns the stream's close error. The Jabra mute
// button counts a playback stream as open from opening to closing, so it
// writes no light while one is open on the Jabra.
func TestOutputHooksBracketTheStream(t *testing.T) {
	for _, closeErr := range []error{nil, errors.New("speaker close failed")} {
		t.Run(fmt.Sprintf("close error %v", closeErr), func(t *testing.T) {
			stream := newFakeStream(48000)
			stream.closeErr = closeErr
			var mu sync.Mutex
			var calls []string
			call := func(c string) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, c)
			}
			opener := &fakeOpener{streams: []*fakeStream{stream}}
			cfg := OutputConfig{
				IdleClose: 10 * time.Millisecond,
				Opening:   func() { call("opening") },
				Closing: func(closeStream func() error) {
					call(fmt.Sprintf("closing (closed before closeStream: %t)", stream.isClosed()))
					err := closeStream()
					call(fmt.Sprintf("closed: %t, error %v", stream.isClosed(), err))
				},
			}
			cfg.Open = func(device string) (PlaybackStream, int, error) {
				call("open")
				return opener.open(device)
			}
			out := NewOutput(cfg)
			t.Cleanup(func() { _ = out.Close() })
			ctx := testContext(t)
			pb := out.Begin(InputVoice, nil)
			if err := pb.WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			if err := pb.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			want := []string{"opening", "open", "closing (closed before closeStream: false)", fmt.Sprintf("closed: true, error %v", closeErr)}
			waitFor(t, "the idle stream to close", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(calls) == len(want)
			})
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(calls, want) {
				t.Fatalf("calls = %q, want %q", calls, want)
			}
		})
	}
}

// TestOutputHooksRunWhenTheStreamDoesNotOpen fails if an open that fails
// leaves the closing hook uncalled, which would count the stream open for good
// so that the Jabra light is never written again, or keeps the failure from
// the writer.
func TestOutputHooksRunWhenTheStreamDoesNotOpen(t *testing.T) {
	var mu sync.Mutex
	opened, closed := 0, 0
	out := newTestOutput(t, OutputConfig{
		Opening: func() { mu.Lock(); opened++; mu.Unlock() },
		Closing: func(closeStream func() error) {
			mu.Lock()
			closed++
			mu.Unlock()
			if err := closeStream(); err != nil {
				t.Errorf("closeStream with no stream opened = %v, want nil", err)
			}
		},
	}, &fakeOpener{err: errors.New("no output device")})
	ctx := testContext(t)
	pb := out.Begin(InputVoice, nil)
	err := pb.WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k)
	if err == nil {
		err = pb.Drain(ctx)
	}
	if !containsText(err, "no output device") {
		t.Fatalf("a playback with no output device = %v, want the open's error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if opened != 1 || closed != 1 {
		t.Fatalf("opening ran %d times and closing %d times, want once each", opened, closed)
	}
}

func containsText(err error, text string) bool {
	return err != nil && strings.Contains(err.Error(), text)
}

// TestOutputLogsAStreamThatFailsToClose fails if an Output with no closing
// hook drops the error of a stream that did not close instead of logging it
// at WARN.
func TestOutputLogsAStreamThatFailsToClose(t *testing.T) {
	var mu sync.Mutex
	var logs strings.Builder
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
	defer slog.SetDefault(original)
	stream := newFakeStream(48000)
	stream.closeErr = errors.New("speaker close failed")
	out := newTestOutput(t, OutputConfig{IdleClose: 10 * time.Millisecond}, &fakeOpener{streams: []*fakeStream{stream}})
	ctx := testContext(t)
	pb := out.Begin(InputVoice, nil)
	if err := pb.WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	if err := pb.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	logged := func() string {
		mu.Lock()
		defer mu.Unlock()
		return logs.String()
	}
	deadline := time.Now().Add(5 * time.Second)
	for !stream.isClosed() || !strings.Contains(logged(), "speaker close failed") {
		if time.Now().After(deadline) {
			t.Fatalf("the stream closed: %t; the log holds %q; want a WARN line with the close's error", stream.isClosed(), logged())
		}
		time.Sleep(time.Millisecond)
	}
	if line := logged(); !strings.Contains(line, "level=WARN") || !strings.Contains(line, `msg="speaker stream close failed"`) {
		t.Fatalf("the log holds %q; want a WARN line, speaker stream close failed", line)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestOutputReportsAStreamFailureToTheWriter fails if a write the device
// refuses, as when the Jabra is unplugged, does not reach the playback's
// writer, or leaves the stream open.
func TestOutputReportsAStreamFailureToTheWriter(t *testing.T) {
	stream := newFakeStream(48000)
	stream.writeErr = errors.New("device disconnected")
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}})
	ctx := testContext(t)
	pb := out.Begin(InputVoice, nil)
	err := pb.WriteAll(ctx, constant(48000, time.Second, 1000), mono48k)
	if err == nil {
		err = pb.Drain(ctx)
	}
	if !containsText(err, "device disconnected") {
		t.Fatalf("a playback on a stream that fails = %v, want the stream's error", err)
	}
	waitFor(t, "the failed stream to close", stream.isClosed)
}

// TestOutputSurvivesAStreamWhoseClosePanics fails if a panic in a stream's
// close ends the Output or keeps the closing hook from finishing its count:
// the next speech must play on a new stream.
func TestOutputSurvivesAStreamWhoseClosePanics(t *testing.T) {
	broken, next := newFakeStream(48000), newFakeStream(48000)
	broken.closePanics = true
	out := newTestOutput(t, OutputConfig{IdleClose: 10 * time.Millisecond}, &fakeOpener{streams: []*fakeStream{broken, next}})
	ctx := testContext(t)
	for _, stream := range []*fakeStream{broken, next} {
		pb := out.Begin(InputVoice, nil)
		if err := pb.WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
			t.Fatal(err)
		}
		if err := pb.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the idle stream to close", stream.isClosed)
	}
	if silent(next.samples()) {
		t.Fatal("the speech after a stream whose close panicked did not play")
	}
}

// TestPlaybackWriteTakesOnlyWholeFramesOf20msOrLess fails if Write takes a
// frame longer than 20 ms, a partial sample, or a format without a rate or
// channels, or if WriteAll does not split a long clip into frames Write takes.
func TestPlaybackWriteTakesOnlyWholeFramesOf20msOrLess(t *testing.T) {
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{newFakeStream(48000)}})
	ctx := testContext(t)
	pb := out.Begin(InputVoice, nil)
	for _, tc := range []struct {
		name   string
		frame  []byte
		format AudioFormat
		want   error
	}{
		{name: "21 ms", frame: constant(48000, 21*time.Millisecond, 1), format: mono48k, want: ErrFrameTooLong},
		{name: "half a sample", frame: []byte{1}, format: mono48k, want: ErrInvalidFormat},
		{name: "half a stereo frame", frame: []byte{1, 0}, format: stereo48k, want: ErrInvalidFormat},
		{name: "no rate", frame: []byte{1, 0}, format: AudioFormat{Channels: 1}, want: ErrInvalidFormat},
		{name: "no channels", frame: []byte{1, 0}, format: AudioFormat{SampleRate: 48000}, want: ErrInvalidFormat},
		{name: "a rate above 384 kHz", frame: []byte{1, 0}, format: AudioFormat{SampleRate: 1_000_000, Channels: 1}, want: ErrInvalidFormat},
		{name: "more than 8 channels", frame: make([]byte, 18), format: AudioFormat{SampleRate: 48000, Channels: 9}, want: ErrInvalidFormat},
		{name: "mp3", frame: []byte{1, 0}, format: AudioFormat{SampleRate: 48000, Channels: 1, Encoding: EncodingMP3}, want: ErrInvalidFormat},
		{name: "20 ms", frame: constant(48000, 20*time.Millisecond, 1), format: mono48k},
	} {
		if err := pb.Write(ctx, tc.frame, tc.format); !errors.Is(err, tc.want) {
			t.Errorf("Write of %s = %v, want %v", tc.name, err, tc.want)
		}
	}
	if err := pb.WriteAll(ctx, constant(48000, 90*time.Millisecond, 1), mono48k); err != nil {
		t.Errorf("WriteAll of 90 ms = %v", err)
	}
}

// TestOutputCloseEndsPlayback fails if Close leaves a playback's writer
// waiting or the stream open, or lets a later playback start.
func TestOutputCloseEndsPlayback(t *testing.T) {
	stream := newFakeStream(48000)
	out := NewOutput(OutputConfig{Open: (&fakeOpener{streams: []*fakeStream{stream}}).open})
	ctx := testContext(t)
	pb := out.Begin(InputVoice, nil)
	written := make(chan error, 1)
	go func() { written <- pb.WriteAll(ctx, constant(48000, 2*time.Second, 1000), mono48k) }()
	stream.waitWriting(t)
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-written; !errors.Is(err, ErrOutputClosed) {
		t.Fatalf("WriteAll under a Close = %v, want ErrOutputClosed", err)
	}
	if !stream.isClosed() {
		t.Fatal("the stream is still open after Close returned")
	}
	if err := out.Begin(InputVoice, nil).Write(ctx, constant(48000, 20*time.Millisecond, 1), mono48k); !errors.Is(err, ErrOutputClosed) {
		t.Fatalf("a write after Close = %v, want ErrOutputClosed", err)
	}
}

// TestPlaybackQueuesNothingUnderADoneContext fails if a Write or WriteAll
// whose context is already done queues audio: a playback begun just after its
// exchange was stopped, so after the Cancel that ended the playbacks before
// it, would still play up to InputQueue.
func TestPlaybackQueuesNothingUnderADoneContext(t *testing.T) {
	stream := newFakeStream(48000)
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}})
	stopped, stop := context.WithCancel(context.Background())
	stop()
	late := out.Begin(InputVoice, nil)
	if err := late.Write(stopped, constant(48000, MaxWriteFrame, 1000), mono48k); !errors.Is(err, context.Canceled) {
		t.Errorf("Write under a done context = %v, want context.Canceled", err)
	}
	if err := late.WriteAll(stopped, constant(48000, InputQueue, 1000), mono48k); !errors.Is(err, context.Canceled) {
		t.Errorf("WriteAll under a done context = %v, want context.Canceled", err)
	}
	// Control: a playback with a live context plays, and only its audio.
	ctx := testContext(t)
	next := out.Begin(InputVoice, nil)
	if err := next.WriteAll(ctx, constant(48000, MaxWriteFrame, 7), mono48k); err != nil {
		t.Fatal(err)
	}
	if err := next.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	queued := 0
	for _, s := range stream.samples() {
		if s == 1000 {
			queued++
		}
	}
	if queued > 0 {
		t.Fatalf("the stream played %d samples written under a done context", queued)
	}
	if silent(stream.samples()) {
		t.Fatal("control: the playback with a live context did not play")
	}
}

// TestPlaybackDrainWaitsForTheStreamToTakeItsLastFrame fails if Drain returns
// while the frame that holds the playback's last audio is still on its way to
// the stream: a writer that drains and then reports the speech done, or reads
// what played, would run ahead of the device.
func TestPlaybackDrainWaitsForTheStreamToTakeItsLastFrame(t *testing.T) {
	stream := newFakeStream(48000)
	stream.block = make(chan struct{})
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}})
	ctx := testContext(t)
	pb := out.Begin(InputVoice, nil)
	if err := pb.Write(ctx, constant(48000, outputFrame, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	stream.waitWriting(t) // the frame that holds all of it is mixed, and its write waits
	drained := make(chan error, 1)
	go func() { drained <- pb.Drain(ctx) }()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned %v while the stream's write of the playback's frame was under way", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(stream.block)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
}

// countingStream plays like fakeStream, in real time, but keeps nothing: it
// only counts its frames, so it allocates nothing itself.
type countingStream struct{ frames atomic.Int64 }

func (s *countingStream) Write(frame []int16) error {
	s.frames.Add(1)
	time.Sleep(time.Duration(len(frame)) * time.Second / 48000)
	return nil
}
func (s *countingStream) Drop() (int, error) { return 0, nil }
func (s *countingStream) Close() error       { return nil }

// TestOutputMixesWithoutAllocating fails if the mixer allocates for each
// frame while a writer waits for room, as one does for the whole of a long
// speech: garbage at 100 frames a second makes the collector run under the
// speech, and a collection can make the device run out.
func TestOutputMixesWithoutAllocating(t *testing.T) {
	stream := &countingStream{}
	out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, 48000, nil }})
	t.Cleanup(func() { _ = out.Close() })
	ctx := testContext(t)
	pb := out.Begin(InputVoice, nil)
	go func() { _ = pb.WriteAll(ctx, constant(48000, 3*time.Second, 1000), mono48k) }()
	time.Sleep(300 * time.Millisecond) // the queue is full and the writer waits
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	frames := stream.frames.Load()
	time.Sleep(500 * time.Millisecond)
	runtime.ReadMemStats(&after)
	frames = stream.frames.Load() - frames
	if frames < 20 {
		t.Fatalf("control: the stream played %d frames in 500 ms", frames)
	}
	// A few allocations come from the runtime and the test; one a frame is
	// the mixer's.
	if allocs := after.Mallocs - before.Mallocs; allocs >= uint64(frames)/2 {
		t.Fatalf("%d allocations while the mixer wrote %d frames, with a writer waiting for room", allocs, frames)
	}
}

// stallingStream never makes a write wait, as ALSA's null PCM, except that
// every fifth write takes 3 ms, as when the process waited for a processor.
// It keeps the most audio it was written ahead of real time.
type stallingStream struct {
	mu      sync.Mutex
	start   time.Time
	written time.Duration
	lead    time.Duration
	writes  int
}

func (s *stallingStream) Write(frame []int16) error {
	s.mu.Lock()
	now := time.Now()
	if s.start.IsZero() {
		s.start = now
	}
	s.written += time.Duration(len(frame)) * time.Second / 48000
	s.lead = max(s.lead, s.written-now.Sub(s.start))
	s.writes++
	stall := s.writes%5 == 0
	s.mu.Unlock()
	if stall {
		time.Sleep(3 * time.Millisecond)
	}
	return nil
}
func (s *stallingStream) Drop() (int, error) { return 0, nil }
func (s *stallingStream) Close() error       { return nil }

// TestOutputKeepsToRealTimeOnAStreamThatNeverWaits fails if the Output runs
// more than outputAhead ahead of real time on a stream that never makes a
// write wait, even when some writes are slow because the process waited, or
// when Cancel empties the stream: completion, speech ends and the stream's
// close would come before the audio's time, and a Cancel cannot take back the
// lead.
func TestOutputKeepsToRealTimeOnAStreamThatNeverWaits(t *testing.T) {
	stream := &stallingStream{}
	out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, 48000, nil }})
	t.Cleanup(func() { _ = out.Close() })
	ctx := testContext(t)
	for range 3 {
		pb := out.Begin(InputVoice, nil)
		go func() { _ = pb.WriteAll(ctx, constant(48000, time.Second, 1000), mono48k) }()
		time.Sleep(300 * time.Millisecond)
		out.Cancel(InputVoice)
	}
	stream.mu.Lock()
	lead := stream.lead
	stream.mu.Unlock()
	// The stream measures as each write begins, counting the frame it is
	// given: the pacer's bound, that frame, and the next one's scheduling.
	if lead > outputAhead+2*outputFrame {
		t.Fatalf("the Output ran %v ahead of real time, want at most %v", lead, outputAhead)
	}
}

// TestOutputKeepsNoFinishedPlayback fails if the Output keeps a playback that
// has played all its audio once the next one has played past the history a
// Drop can give back, so that a process that speaks for weeks grows by one
// playback per speech. Until then the history must name it: a Cancel can
// still take its audio back from the device, off what it played.
func TestOutputKeepsNoFinishedPlayback(t *testing.T) {
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{newFakeStream(48000)}})
	ctx := testContext(t)
	play := func(d time.Duration) *Playback {
		pb := out.Begin(InputVoice, nil)
		if err := pb.WriteAll(ctx, constant(48000, d, 1000), mono48k); err != nil {
			t.Fatal(err)
		}
		if err := pb.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		return pb
	}
	first := weak.Make(play(outputFrame))
	play(outputHistory + 2*outputFrame)
	for range 5 {
		runtime.GC()
	}
	if first.Value() != nil {
		t.Fatal("the Output still holds a playback that played all its audio, after the next one played past the history")
	}
}
