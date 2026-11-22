//go:build linux

package audio

import (
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// discardingStream gives an account of its playback (playPosition) on its own
// clock, and at its first write discards audio, as a PCM that suspends does:
// with keep it had played the first half of that frame and gives back only
// the second half (takeAgain); without it, it gives back the whole frame, of
// which it played nothing. Its clock moves on 1 s across the suspend.
type discardingStream struct {
	mu      sync.Mutex
	discard bool
	keep    bool
	writes  int
	again   int
	now     int64
	delay   int
	first   int64
}

func (s *discardingStream) Write(frame []int16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.writes == 1 {
		s.first = s.now
		if s.discard {
			s.again = len(frame)
			if s.keep {
				s.again = len(frame) / 2
			}
			s.now += int64(time.Second)
			s.delay = 0
			return nil
		}
	}
	s.delay = len(frame)
	return nil
}

func (s *discardingStream) takeAgain() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.again
	s.again = 0
	return n
}

func (s *discardingStream) playPosition() (playPosition, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return playPosition{running: true, delay: s.delay, before: s.now, after: s.now, wall: time.Unix(0, s.now)}, true
}

func (*discardingStream) Drop() (int, error) { return 0, nil }
func (*discardingStream) Close() error       { return nil }

// TestAnAudibleTimeIsGivenOnlyWithNoDisruptionAtItsFirstWrite fails if a
// speech whose first write the device discarded, in part or in whole, is
// timed at all. With half the frame played before the discard, the replay's
// account puts the first sample a second late here (Sol's review of #154 at
// 7d27574a): a late chime shortens the time from the chime to the answer and
// can hide a leak. A replay is a disruption, so no speech it came to is timed,
// even one whose first sample it wrote again (Sol at 7bf1ae31: whether that
// sample was unplayed is an estimate). The control, with no discard, is timed
// at its write.
func TestAnAudibleTimeIsGivenOnlyWithNoDisruptionAtItsFirstWrite(t *testing.T) {
	for _, tc := range []struct {
		name          string
		discard, keep bool
	}{
		{"no discard", false, false},
		{"the whole first frame discarded unplayed", true, false},
		{"half the first frame played before the discard", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &discardingStream{discard: tc.discard, keep: tc.keep, now: int64(10 * time.Second)}
			out, start, _ := probeOutput(t, stream)
			events := make(chan AudibleEvent, 16)
			out.OnAudible(func(e AudibleEvent) { events <- e })
			if err := out.Begin(InputChime, nil).Write(testContext(t), constant(48000, 10*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			start()
			select {
			case e := <-events:
				stream.mu.Lock()
				first := stream.first
				stream.mu.Unlock()
				if tc.discard {
					t.Fatalf("the chime was timed at %d across a discard of its first write, which began at %d: %+v", e.At.Mono, first, e)
				}
				if e.At.Mono != first || !e.At.Wall.Equal(time.Unix(0, first)) {
					t.Fatalf("control: the chime's first sample left the buffer at %d; the event says %+v", first, e)
				}
			case <-time.After(300 * time.Millisecond):
				if !tc.discard {
					t.Fatal("control: no audible event for the chime")
				}
			}
		})
	}
}

// suspendingPCM is a fakePCM that, at the first write carrying the chime,
// takes 240 samples of it, lets the device take 120 of those, and suspends
// with the other 120 still queued, for 150 ms (GPT-6.1 Sol's probes of #154):
// the room has heard what the device took less what is in flight. With
// inflightThen set, what is in flight shrinks to it before the suspend, so
// the stream's last count, from the snapshot before, says more was in flight
// than was: a resume that fails then replays from before the chime's first
// sample, which the room had heard. resumes (fakePCM) makes the resume
// succeed.
type suspendingPCM struct {
	*fakePCM
	stream       *alsaStream
	inflightThen int
	fired        bool // only writei, on the mixer goroutine

	stampMu     sync.Mutex
	suspendedAt int64
	heardBefore int
	estimate    int
}

func (p *suspendingPCM) writei(samples []int16, frames int) int {
	if p.fired || samples[0] <= 1000 {
		return p.fakePCM.writei(samples, frames)
	}
	p.fired = true
	estimate := p.stream.inflight
	n := p.fakePCM.writei(samples[:240], 240)
	if n <= 0 {
		return n
	}
	// Advance the device, not the process clock: it took 120 samples of the
	// chime, and the rest stay queued when it suspends.
	p.mu.Lock()
	run := p.runs[len(p.runs)-1]
	target := len(run.samples) - 120
	p.clock += target - run.taken
	run.taken = target
	if p.inflightThen > 0 {
		p.inflight = p.inflightThen
	}
	p.mu.Unlock()
	p.suspend()
	at, _ := monotonicNanos()
	prefix := 0
	p.heard(func(sample int16, _ time.Time) {
		if sample > 1000 {
			prefix++
		}
	})
	p.stampMu.Lock()
	p.suspendedAt, p.heardBefore, p.estimate = at, prefix, estimate
	p.stampMu.Unlock()
	time.Sleep(150 * time.Millisecond)
	return n
}

// TestAnAudibleTimeIsNotGivenAcrossASuspend fails if, through the ALSA
// stream, a chime whose first samples the PCM played before it suspended is
// timed at all. Each account after the suspend puts its first sample after
// the suspend, though the room heard it before (Sol's probes of #154):
//   - the resume fails, and the replay carries only the rest: 144 to 156 ms
//     late at 7d27574a;
//   - the resume succeeds: the queue keeps its samples but not their timing,
//     148 to 153 ms late at 7bf1ae31;
//   - the resume fails, and the stream's count of what was in flight, from the
//     snapshot before the suspend, is 48 frames more than was: the replay
//     starts 24 frames before the chime, so its first sample seems unplayed,
//     though 24 had played; 152 to 159 ms late at 7bf1ae31.
//
// No time is right for it, so the chime must have no audible event, and the
// acceptance driver falls back with a NOTE.
func TestAnAudibleTimeIsNotGivenAcrossASuspend(t *testing.T) {
	for _, tc := range []struct {
		name                string
		resumes             bool
		inflight, inflight2 int
		heard               int
	}{
		{name: "the resume fails", heard: 120},
		{name: "the resume succeeds", resumes: true, heard: 120},
		{name: "the resume fails, with less in flight than counted", inflight: 144, inflight2: 96, heard: 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pcm := &suspendingPCM{fakePCM: newFakePCM(48000, 3840), inflightThen: tc.inflight2}
			pcm.lockstep, pcm.resumes, pcm.inflight = true, tc.resumes, tc.inflight
			stream := newALSAStream(pcm, "suspending", alsaParams{rate: 48000, channels: 1, period: 480, buffer: 3840})
			pcm.stream = stream
			out, start, _ := probeOutput(t, stream)
			events := make(chan AudibleEvent, 16)
			out.OnAudible(func(e AudibleEvent) { events <- e })
			ctx := testContext(t)
			if err := out.Begin("bed", nil).WriteAll(ctx, constant(48000, 100*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			start()
			select {
			case <-events:
			case <-time.After(time.Second):
				t.Fatal("fixture: the bed under the chime did not start")
			}
			if tc.inflight > 0 {
				waitFor(t, "a full in-flight queue", func() bool {
					pcm.mu.Lock()
					defer pcm.mu.Unlock()
					return pcm.clock >= 960
				})
			}
			if err := out.Begin(InputChime, nil).Write(ctx, constant(48000, 10*time.Millisecond, 3000), mono48k); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(time.Second)
			for {
				select {
				case e := <-events:
					if e.Input != InputChime {
						continue
					}
					pcm.stampMu.Lock()
					suspended, prefix := pcm.suspendedAt, pcm.heardBefore
					pcm.stampMu.Unlock()
					t.Fatalf("the chime was timed at %d, %v after the PCM suspended, though it had played %d of its samples before: %+v", e.At.Mono, time.Duration(e.At.Mono-suspended), prefix, e)
				case <-deadline:
					pcm.stampMu.Lock()
					suspended, prefix, estimate := pcm.suspendedAt, pcm.heardBefore, pcm.estimate
					pcm.stampMu.Unlock()
					if suspended == 0 || prefix != tc.heard || (tc.inflight > 0 && estimate != tc.inflight) {
						t.Fatalf("fixture: suspended at %d, with %d of the chime's samples played before (want %d), the stream counting %d in flight (want %d)", suspended, prefix, tc.heard, estimate, tc.inflight)
					}
					return
				}
			}
		})
	}
}

// betweenWritesPCM is a fakePCM that suspends right after it took a whole
// write: the chime's second, so that its first sample is still queued and no
// write has yet found the suspend (Sol's probe of #154 at ce454880). The next
// write waits for release, so no recovery hides the state.
type betweenWritesPCM struct {
	*fakePCM
	suspendIt   bool
	chimeWrites int
	fired       bool
	release     chan struct{}

	stampMu     sync.Mutex
	suspendedAt int64
	heardChime  int
}

func (p *betweenWritesPCM) writei(samples []int16, frames int) int {
	if p.fired {
		<-p.release
	}
	n := p.fakePCM.writei(samples, frames)
	if n <= 0 || !p.suspendIt {
		return n
	}
	if p.chimeWrites > 0 || samples[0] > 1000 {
		p.chimeWrites++
	}
	if p.chimeWrites == 2 && n == frames && !p.fired {
		p.fired = true
		p.suspend()
		at, _ := monotonicNanos()
		heard := 0
		p.heard(func(v int16, _ time.Time) {
			if v > 1000 {
				heard++
			}
		})
		p.stampMu.Lock()
		p.suspendedAt, p.heardChime = at, heard
		p.stampMu.Unlock()
		time.Sleep(150 * time.Millisecond)
	}
	return n
}

// TestAnAudibleTimeIsNotGivenAcrossASuspendBetweenWrites fails if a chime
// whose first sample was still queued when the PCM suspended, after a write
// it took whole, is timed: no write found the suspend, and the event, held
// for its time, was reported 63 to 73 ms after the suspend (Sol's review of
// #154 at ce454880). The control, with no suspend, is timed.
func TestAnAudibleTimeIsNotGivenAcrossASuspendBetweenWrites(t *testing.T) {
	for _, suspend := range []bool{false, true} {
		t.Run(map[bool]string{false: "no suspend", true: "a suspend after a whole write"}[suspend], func(t *testing.T) {
			pcm := &betweenWritesPCM{fakePCM: newFakePCM(48000, 3840), suspendIt: suspend, release: make(chan struct{})}
			pcm.lockstep, pcm.inflight = true, 144
			stream := newALSAStream(pcm, "between-writes", alsaParams{rate: 48000, channels: 1, period: 480, buffer: 3840})
			out, start, _ := probeOutput(t, stream)
			t.Cleanup(func() { close(pcm.release) })
			events := make(chan AudibleEvent, 16)
			out.OnAudible(func(e AudibleEvent) { events <- e })
			ctx := testContext(t)
			if err := out.Begin("bed", nil).WriteAll(ctx, constant(48000, 100*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			start()
			select {
			case <-events:
			case <-time.After(time.Second):
				t.Fatal("fixture: the bed under the chime was not timed")
			}
			if err := out.Begin(InputChime, nil).Write(ctx, constant(48000, 10*time.Millisecond, 3000), mono48k); err != nil {
				t.Fatal(err)
			}
			select {
			case e := <-events:
				if e.Input != InputChime {
					t.Fatalf("fixture: event %+v", e)
				}
				if suspend {
					pcm.stampMu.Lock()
					at, heard := pcm.suspendedAt, pcm.heardChime
					pcm.stampMu.Unlock()
					t.Fatalf("the chime was timed %v after the PCM suspended, with %d of its samples heard before: %+v", time.Duration(e.At.Mono-at), heard, e)
				}
			case <-time.After(time.Second):
				if !suspend {
					t.Fatal("control: the chime was not timed")
				}
				pcm.stampMu.Lock()
				at, heard := pcm.suspendedAt, pcm.heardChime
				pcm.stampMu.Unlock()
				if at == 0 || heard != 0 {
					t.Fatalf("fixture: suspended at %d, with %d of the chime's samples heard before, want 0", at, heard)
				}
			}
		})
	}
}

// restartingPCM is a sinkPCM that runs with half a second queued, and whose
// trigger timestamp, ALSA's time of its last start, stop, suspend or resume,
// the test moves, as a restart no write of the stream's own found would.
type restartingPCM struct {
	*sinkPCM
	mu      sync.Mutex
	trigger int64
}

func (p *restartingPCM) status() pcmStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return pcmStatus{running: true, delay: 24000, avail: p.buffer - 24000, trigger: p.trigger}
}

// TestAnAudibleTimeIsNotGivenAcrossARestartOfTheStream fails if a held event
// is reported when the stream, read as its time passes, runs from another
// trigger than the account that timed it: it stopped or suspended and started
// again between the two reads, and the account's time no longer holds. The
// control, with the same trigger, is reported.
func TestAnAudibleTimeIsNotGivenAcrossARestartOfTheStream(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "the same trigger", true: "another trigger"}[restart], func(t *testing.T) {
			pcm := &restartingPCM{sinkPCM: &sinkPCM{buffer: 28800}, trigger: 1_000_000_000}
			stream := newALSAStream(pcm, "restarting", alsaParams{rate: 48000, channels: 1, period: 480, buffer: 28800})
			out, start, _ := probeOutput(t, stream)
			events := make(chan AudibleEvent, 16)
			out.OnAudible(func(e AudibleEvent) { events <- e })
			// A second of chime keeps the stream playing past the half second queued, so that it
			// does not close idle before the chime's time comes.
			pb := out.Begin(InputChime, nil)
			go func() { _ = pb.WriteAll(testContext(t), constant(48000, time.Second, 1000), mono48k) }()
			start()
			waitFor(t, "the chime to be timed", func() bool {
				out.mu.Lock()
				defer out.mu.Unlock()
				return len(out.timed) == 1
			})
			if restart {
				pcm.mu.Lock()
				pcm.trigger += int64(time.Second)
				pcm.mu.Unlock()
			}
			select {
			case e := <-events:
				if restart {
					t.Fatalf("the chime was timed across a restart of the stream: %+v", e)
				}
			case <-time.After(1500 * time.Millisecond):
				if !restart {
					t.Fatal("control: the chime was not timed")
				}
			}
		})
	}
}

// TestOrdinaryStartsAreTimed fails if a chime played through the ALSA stream
// with nothing disrupting it is not timed, every time, with no disruption, or
// if its time's two clocks do not agree with the clocks read as it comes: the
// rule that drops a time across any disruption must not drop the ordinary
// start (Sol's census of #154 at 7bf1ae31: 20 of 20 timed).
func TestOrdinaryStartsAreTimed(t *testing.T) {
	const trials = 20
	for i := range trials {
		pcm := newFakePCM(48000, 3840)
		pcm.lockstep, pcm.inflight = true, 144
		stream := newALSAStream(pcm, "ordinary", alsaParams{rate: 48000, channels: 1, period: 480, buffer: 3840})
		out, start, _ := probeOutput(t, stream)
		type heard struct {
			event AudibleEvent
			at    monoclock.Stamp
		}
		events := make(chan heard, 16)
		out.OnAudible(func(e AudibleEvent) { events <- heard{e, monoclock.Now()} })
		if err := out.Begin(InputChime, nil).Write(testContext(t), constant(48000, 10*time.Millisecond, 1000), mono48k); err != nil {
			t.Fatal(err)
		}
		start()
		select {
		case h := <-events:
			if h.event.Input != InputChime || h.event.Disruptions != 0 {
				t.Fatalf("trial %d: event %+v, want the chime with no disruption", i, h.event)
			}
			if h.at.Mono < h.event.At.Mono {
				t.Fatalf("trial %d: the event came %v before its time", i, time.Duration(h.event.At.Mono-h.at.Mono))
			}
			wall, mono := h.at.Wall.Sub(h.event.At.Wall), time.Duration(h.at.Mono-h.event.At.Mono)
			if d := wall - mono; d > time.Millisecond || d < -time.Millisecond {
				t.Fatalf("trial %d: the event came %v after its time on the wall clock, %v on CLOCK_MONOTONIC", i, wall, mono)
			}
		case <-time.After(time.Second):
			t.Fatalf("trial %d: an ordinary start gave no audible event", i)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// failingStatusPCM is a sinkPCM whose status fails while fail is set.
type failingStatusPCM struct {
	*sinkPCM
	fail bool
}

func (p *failingStatusPCM) status() pcmStatus {
	if p.fail {
		return pcmStatus{running: true, delay: 480, rc: -int(syscall.EIO)}
	}
	return pcmStatus{running: true, delay: 480}
}

// TestAFailedAccountTimesNothing fails if a status snapshot that fails times
// the speech that waits, or lets it stop waiting, or if the next snapshot,
// which succeeds, does not time it (Sol's probe of #154 at 7d27574a).
func TestAFailedAccountTimesNothing(t *testing.T) {
	pcm := &failingStatusPCM{sinkPCM: &sinkPCM{buffer: 3840}, fail: true}
	stream := newALSAStream(pcm, "failing", alsaParams{rate: 48000, channels: 1, period: 480, buffer: 3840})
	in := &outputInput{name: InputChime, audiblePending: true}
	out := &Output{order: []*outputInput{in}, pos: 480, eventWake: make(chan struct{}, 1)}
	out.timeAudible(stream, 48000)
	if len(out.timed) != 0 || !in.audiblePending {
		t.Fatalf("a failed status timed %+v, or the speech stopped waiting (%v)", out.timed, !in.audiblePending)
	}
	pcm.fail = false
	out.timeAudible(stream, 48000)
	if len(out.timed) != 1 || in.audiblePending {
		t.Fatalf("a successful status did not time the waiting speech: %+v", out.timed)
	}
}
