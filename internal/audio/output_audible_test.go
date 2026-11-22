package audio

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// clockedStream is a fakeStream that gives an account of its playback as an
// ALSA stream does (playPosition): a fixed delay, whether it runs, and a clock
// read around the account, each read recorded with the positions written by
// then. It counts underruns as the test sets them.
type clockedStream struct {
	*fakeStream
	delay int

	mu       sync.Mutex
	written  int64
	running  bool
	underrun int
	clock    int64
	reads    []positionRead
}

// positionRead is one account the stream gave, with what it had been written.
type positionRead struct {
	running       bool
	written       int64
	before, after int64
}

func newClockedStream(delay int) *clockedStream {
	return &clockedStream{fakeStream: newFakeStream(48000), delay: delay}
}

func (s *clockedStream) Write(frame []int16) error {
	err := s.fakeStream.Write(frame)
	s.mu.Lock()
	s.written += int64(len(frame))
	s.mu.Unlock()
	return err
}

// clockedWall is the wall clock a clockedStream reads with its account: an
// instant that moves with its fake CLOCK_MONOTONIC.
var clockedWall = time.Unix(1_800_000_000, 0)

func (s *clockedStream) playPosition() (playPosition, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock += int64(time.Millisecond)
	r := positionRead{running: s.running, written: s.written, before: s.clock, after: s.clock + 2000}
	s.reads = append(s.reads, r)
	return playPosition{running: r.running, delay: s.delay, before: r.before, after: r.after, wall: clockedWall.Add(time.Duration(r.before))}, true
}

func (s *clockedStream) underruns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.underrun
}

func (s *clockedStream) set(running bool, underruns int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running, s.underrun = running, underruns
}

func (s *clockedStream) accounts() []positionRead {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reads)
}

// audibleRig is an Output on a clockedStream, with its speech and audible
// events.
type audibleRig struct {
	t      *testing.T
	out    *Output
	stream *clockedStream
	speech chan SpeechEvent

	mu       sync.Mutex
	audibles []AudibleEvent
}

func newAudibleRig(t *testing.T, running bool) *audibleRig {
	t.Helper()
	r := &audibleRig{t: t, stream: newClockedStream(4800), speech: make(chan SpeechEvent, 16)}
	r.stream.set(running, 0)
	r.out = NewOutput(OutputConfig{IdleClose: 10 * time.Second, Open: func(string) (PlaybackStream, int, error) {
		return r.stream, 48000, nil
	}})
	t.Cleanup(func() { _ = r.out.Close() })
	r.out.OnSpeech(func(e SpeechEvent) { r.speech <- e })
	r.out.OnAudible(func(e AudibleEvent) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.audibles = append(r.audibles, e)
	})
	return r
}

// play queues d of a constant value on input, at rate, and waits for its
// speech to start.
func (r *audibleRig) play(input string, rate int, d time.Duration, value int16) {
	r.t.Helper()
	pb := r.out.Begin(input, nil)
	ctx := testContext(r.t)
	go func() {
		_ = pb.WriteAll(ctx, constant(rate, d, value), AudioFormat{SampleRate: rate, Channels: 1})
	}()
	r.waitSpeech(input, true)
}

func (r *audibleRig) waitSpeech(input string, started bool) SpeechEvent {
	r.t.Helper()
	for {
		select {
		case e := <-r.speech:
			if e.Input == input && e.Started == started {
				return e
			}
		case <-time.After(5 * time.Second):
			r.t.Fatalf("no speech event (%s, started %v)", input, started)
		}
	}
}

func (r *audibleRig) audible(input string) []AudibleEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var got []AudibleEvent
	for _, e := range r.audibles {
		if e.Input == input {
			got = append(got, e)
		}
	}
	return got
}

func (r *audibleRig) waitAudible(input string) AudibleEvent {
	r.t.Helper()
	var got []AudibleEvent
	waitFor(r.t, "an audible event on "+input, func() bool {
		got = r.audible(input)
		return len(got) > 0
	})
	if len(got) != 1 {
		r.t.Fatalf("%d audible events on %s, want 1: %+v", len(got), input, got)
	}
	return got[0]
}

// firstNonzero is the stream position of the first sample the stream played
// at or after from that is not silence.
func (r *audibleRig) firstNonzero(from int64) int64 {
	r.t.Helper()
	samples := r.stream.samples()
	for p := from; p < int64(len(samples)); p++ {
		if samples[p] != 0 {
			return p
		}
	}
	r.t.Fatalf("no sound in the stream from position %d", from)
	return -1
}

// timedBy finds the stream's account, while it ran, that times position p as
// e.At: its read's middle plus p less what it had played (written less its
// delay), at 48 kHz. It fails the test when none does, or when e.At's wall
// time is not its CLOCK_MONOTONIC moved as the account's wall clock is.
func (r *audibleRig) timedBy(e *AudibleEvent, p int64) positionRead {
	r.t.Helper()
	if !e.At.Wall.Equal(clockedWall.Add(time.Duration(e.At.Mono))) {
		r.t.Fatalf("the event's wall time %v does not go with its CLOCK_MONOTONIC %d: want %v", e.At.Wall, e.At.Mono, clockedWall.Add(time.Duration(e.At.Mono)))
	}
	for _, a := range r.stream.accounts() {
		mid := a.before + (a.after-a.before)/2
		if a.running && e.At.Mono == mid+(p-(a.written-int64(r.stream.delay)))*int64(time.Second)/48000 {
			return a
		}
	}
	r.t.Fatalf("no account of the stream, while it ran, times position %d as %d: %+v", p, e.At.Mono, r.stream.accounts())
	return positionRead{}
}

// TestAudibleEventsTimeEachSpeechsFirstSampleByTheStreamsAccount fails if an
// audible event is not, for the chime and then the voice, the time the
// stream's own account gives the speech's first sample: the account's clock,
// plus the sample's position less what the stream had played (what it was
// written less its delay), at its rate; or if it carries a lead for an input
// played at the stream's rate, or another error than half the account's read.
// The acceptance driver times the answer against the chime with these.
func TestAudibleEventsTimeEachSpeechsFirstSampleByTheStreamsAccount(t *testing.T) {
	r := newAudibleRig(t, true)
	r.play(InputChime, 48000, 100*time.Millisecond, 1000)
	chime := r.waitAudible(InputChime)
	r.waitSpeech(InputChime, false)
	chimeAt := r.firstNonzero(0)
	r.timedBy(&chime, chimeAt)

	r.play(InputVoice, 48000, 100*time.Millisecond, 3000)
	voice := r.waitAudible(InputVoice)
	voiceAt := r.firstNonzero(chimeAt + 4800)
	r.timedBy(&voice, voiceAt)

	for _, e := range []AudibleEvent{chime, voice} {
		if e.Lead != 0 || e.Err != 1001 || e.Disruptions != 0 {
			t.Fatalf("%s: lead %d, error %d, disruptions %d; want 0, 1001 (half the 2 us read, and 1 ns), 0", e.Input, e.Lead, e.Err, e.Disruptions)
		}
	}
}

// TestASpeechsEventsShareItsNumber fails if a speech's start, its end and its
// audible event do not carry the same number, or if two speeches, on the
// chime and then the voice, share one: the acceptance driver times an answer
// by its own speech's audible line, which it ties to the answer's
// first_audio by that number (Sol's review of #156 at 053df77c: the earliest
// voice line near first_audio could be a cancelled word's).
func TestASpeechsEventsShareItsNumber(t *testing.T) {
	r := newAudibleRig(t, true)
	pb := r.out.Begin(InputChime, nil)
	go func() { _ = pb.WriteAll(testContext(t), constant(48000, 100*time.Millisecond, 1000), mono48k) }()
	chimeStart := r.waitSpeech(InputChime, true)
	chime := r.waitAudible(InputChime)
	chimeEnd := r.waitSpeech(InputChime, false)
	pb = r.out.Begin(InputVoice, nil)
	go func() { _ = pb.WriteAll(testContext(t), constant(48000, 100*time.Millisecond, 3000), mono48k) }()
	voiceStart := r.waitSpeech(InputVoice, true)
	voice := r.waitAudible(InputVoice)
	voiceEnd := r.waitSpeech(InputVoice, false)
	for _, s := range []struct {
		name              string
		start, end, heard uint64
	}{
		{"the chime", chimeStart.Speech, chimeEnd.Speech, chime.Speech},
		{"the voice", voiceStart.Speech, voiceEnd.Speech, voice.Speech},
	} {
		if s.start == 0 || s.end != s.start || s.heard != s.start {
			t.Fatalf("%s: start %d, end %d, audible %d; want one number, not 0", s.name, s.start, s.end, s.heard)
		}
	}
	if chimeStart.Speech == voiceStart.Speech {
		t.Fatalf("the chime and the voice share speech %d", chimeStart.Speech)
	}
}

// TestAudibleEventWaitsForTheStreamToRun fails if a speech is timed while the
// stream does not run, as one still filling to its start threshold, which
// starts later than its delay says; or if, once it runs, its time is not the
// stream's account then, of a sample it may already have played.
func TestAudibleEventWaitsForTheStreamToRun(t *testing.T) {
	r := newAudibleRig(t, false)
	r.play(InputChime, 48000, 300*time.Millisecond, 1000)
	time.Sleep(100 * time.Millisecond)
	if got := r.audible(InputChime); len(got) > 0 {
		t.Fatalf("audible events while the stream did not run: %+v", got)
	}
	ran := len(r.stream.accounts())
	r.stream.set(true, 0)
	chime := r.waitAudible(InputChime)
	a := r.timedBy(&chime, r.firstNonzero(0))
	if !slices.Contains(r.stream.accounts()[ran:], a) {
		t.Fatalf("the chime was timed by an account from before the stream ran: %+v", a)
	}
}

// TestAudibleEventSkipsASpeechADisruptionCameTo fails if a speech is timed
// after an underrun came between its start and the account, which can make it
// play later than the account says; or if the next speech's event does not
// count that disruption, which the acceptance driver compares between the
// chime and the answer.
func TestAudibleEventSkipsASpeechADisruptionCameTo(t *testing.T) {
	r := newAudibleRig(t, false)
	r.play(InputChime, 48000, 300*time.Millisecond, 1000)
	r.stream.set(false, 1)
	waitFor(t, "the underrun to be counted", func() bool {
		r.out.mu.Lock()
		defer r.out.mu.Unlock()
		return r.out.disruptions == 1
	})
	r.stream.set(true, 1)
	r.waitSpeech(InputChime, false)
	if got := r.audible(InputChime); len(got) > 0 {
		t.Fatalf("the chime was timed across an underrun: %+v", got)
	}
	r.play(InputVoice, 48000, 100*time.Millisecond, 3000)
	if voice := r.waitAudible(InputVoice); voice.Disruptions != 1 {
		t.Fatalf("the voice's event counts %d disruptions, want the underrun's 1", voice.Disruptions)
	}
}

// TestAudibleEventOfAResampledInputLeadsByItsFilter fails if a resampled
// input's event does not time where its first sample lands, at the centre of
// the resampler's filter, or gives no lead, or one short of where the filter's
// output first becomes sound: the acceptance driver takes At as the chime's
// first sample and At less Lead as the earliest the voice can be heard. A
// step's output reaches half its height at the filter's centre: there the
// first sample lands.
func TestAudibleEventOfAResampledInputLeadsByItsFilter(t *testing.T) {
	r := newAudibleRig(t, true)
	r.play(InputVoice, 24000, 100*time.Millisecond, 3000)
	voice := r.waitAudible(InputVoice)
	if voice.Lead <= 0 || voice.Lead > int64(2*time.Millisecond) {
		t.Fatalf("a voice at 24 kHz on a 48 kHz stream leads by %v, want more than 0 and at most 2 ms", time.Duration(voice.Lead))
	}
	samples := r.stream.samples()
	first, half := r.firstNonzero(0), int64(-1)
	for p := first; p < int64(len(samples)); p++ {
		if samples[p] >= 1500 {
			half = p
			break
		}
	}
	if half < 0 {
		t.Fatal("the voice never reached half its height")
	}
	for _, a := range r.stream.accounts() {
		if !a.running {
			continue
		}
		mid := a.before + (a.after-a.before)/2
		timed := (voice.At.Mono-mid)*48000/int64(time.Second) + a.written - int64(r.stream.delay)
		lead := (voice.Lead*48000 + int64(time.Second) - 1) / int64(time.Second)
		if timed >= half-3 && timed <= half+3 && first >= timed-lead {
			return
		}
	}
	t.Fatalf("no account of the stream times position %d, where the voice reaches half its height, within 3 samples as %d, with its first sound, at %d, no earlier than its lead of %v before: %+v",
		half, voice.At.Mono, first, time.Duration(voice.Lead), r.stream.accounts())
}

// liveStream is a clockedStream whose account reads the real clocks, as an
// ALSA stream's does, so that its times lie ahead of now by its delay.
type liveStream struct{ *clockedStream }

func (s *liveStream) playPosition() (playPosition, bool) {
	before := monoclock.Read()
	wall := time.Now()
	s.mu.Lock()
	r := positionRead{running: s.running, written: s.written, before: before}
	s.mu.Unlock()
	r.after = monoclock.Read()
	s.mu.Lock()
	s.reads = append(s.reads, r)
	s.mu.Unlock()
	return playPosition{running: r.running, delay: s.delay, before: r.before, after: r.after, wall: wall}, true
}

// endingStream is a liveStream whose second write waits for stop, then fails
// when fail is set; gone closes when the Output closes it.
type endingStream struct {
	*liveStream
	writes int // only the mixer's goroutine writes
	stop   chan struct{}
	gone   chan struct{}
	once   sync.Once
	fail   bool
}

func (s *endingStream) Write(frame []int16) error {
	s.writes++
	if s.writes == 2 {
		<-s.stop
		if s.fail {
			return errors.New("the test's stream failed")
		}
	}
	return s.liveStream.Write(frame)
}

func (s *endingStream) Close() error {
	s.once.Do(func() { close(s.gone) })
	return nil
}

// TestHeldAudibleEventsGoWithTheStream fails if an audible event still held
// for its time outlives its stream, closed by a failure, a teardown, Close or
// being idle (Sol's review of #154 at ce454880): no account of a closed
// stream says when it plays. Nothing held may be reported after either.
func TestHeldAudibleEventsGoWithTheStream(t *testing.T) {
	for _, how := range []string{"idle", "failure", "teardown", "close"} {
		t.Run(how, func(t *testing.T) {
			clock := newClockedStream(24000) // 500 ms queued
			clock.set(true, 0)
			stream := &endingStream{liveStream: &liveStream{clock}, stop: make(chan struct{}), gone: make(chan struct{}), fail: how == "failure"}
			out := NewOutput(OutputConfig{IdleClose: 20 * time.Millisecond, Open: func(string) (PlaybackStream, int, error) {
				return stream, 48000, nil
			}})
			var released sync.Once
			unblock := func() { released.Do(func() { close(stream.stop) }) }
			t.Cleanup(func() {
				unblock()
				if err := out.Close(); err != nil {
					t.Error(err)
				}
			})
			got := make(chan AudibleEvent, 16)
			out.OnAudible(func(e AudibleEvent) { got <- e })
			if err := out.Begin(InputChime, nil).Write(testContext(t), constant(48000, 10*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "a held audible event", func() bool {
				out.mu.Lock()
				defer out.mu.Unlock()
				return len(out.timed) == 1
			})
			switch how {
			case "teardown":
				out.stopForTeardown()
			case "close":
				done := make(chan error, 1)
				go func() { done <- out.Close() }()
				waitFor(t, "Close to begin", func() bool {
					out.mu.Lock()
					defer out.mu.Unlock()
					return out.closed
				})
				unblock()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			select {
			case <-stream.gone:
			case <-time.After(5 * time.Second):
				t.Fatal("fixture: the stream did not close")
			}
			out.mu.Lock()
			held := len(out.timed)
			out.mu.Unlock()
			if held != 0 {
				t.Fatalf("%d audible events still held after the stream closed (%s)", held, how)
			}
			select {
			case e := <-got:
				t.Fatalf("a held audible event was reported after its stream closed (%s): %+v", how, e)
			case <-time.After(600 * time.Millisecond):
			}
		})
	}
}

// TestAudibleEventWaitsForItsTimeToPassUndisrupted fails if a speech's
// audible event is reported before its first sample's time has passed, or at
// all when an underrun came after the event was timed and before that time:
// a sample still queued then may play later than the account said (Sol's
// review of #154 at 7bf1ae31: nothing may disrupt the stream between the
// write that carries the sample and its time). The control, with no
// underrun, is reported once its time has passed, and its wall time goes
// with its CLOCK_MONOTONIC.
func TestAudibleEventWaitsForItsTimeToPassUndisrupted(t *testing.T) {
	for _, underrun := range []bool{false, true} {
		t.Run(map[bool]string{false: "undisrupted", true: "an underrun before its time"}[underrun], func(t *testing.T) {
			stream := &liveStream{newClockedStream(9600)} // 200 ms queued
			stream.set(true, 0)
			out := NewOutput(OutputConfig{IdleClose: 10 * time.Second, Open: func(string) (PlaybackStream, int, error) {
				return stream, 48000, nil
			}})
			t.Cleanup(func() { _ = out.Close() })
			type heard struct {
				event AudibleEvent
				at    monoclock.Stamp
			}
			got := make(chan heard, 4)
			out.OnAudible(func(e AudibleEvent) { got <- heard{e, monoclock.Now()} })
			started := make(chan struct{}, 4)
			out.OnSpeech(func(e SpeechEvent) {
				if e.Input == InputChime && e.Started {
					started <- struct{}{}
				}
			})
			pb := out.Begin(InputChime, nil)
			go func() { _ = pb.WriteAll(testContext(t), constant(48000, 50*time.Millisecond, 1000), mono48k) }()
			// The chime is timed at the write its speech starts at, about 190 ms before its
			// first sample leaves the 200 ms queue.
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("fixture: the chime did not start")
			}
			if underrun {
				stream.set(true, 1)
			}
			select {
			case h := <-got:
				switch {
				case underrun:
					t.Fatalf("the chime was reported across an underrun before its time: %+v", h.event)
				case h.at.Mono < h.event.At.Mono:
					t.Fatalf("the chime was reported %v before its time", time.Duration(h.event.At.Mono-h.at.Mono))
				}
				wall, mono := h.at.Wall.Sub(h.event.At.Wall), time.Duration(h.at.Mono-h.event.At.Mono)
				if d := wall - mono; d > time.Millisecond || d < -time.Millisecond {
					t.Fatalf("the event came %v after its time on the wall clock, %v on CLOCK_MONOTONIC: its two clocks do not agree", wall, mono)
				}
			case <-time.After(time.Second):
				if !underrun {
					t.Fatal("the chime was never reported")
				}
			}
		})
	}
}
