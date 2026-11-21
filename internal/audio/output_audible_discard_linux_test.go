//go:build linux

package audio

import (
	"sync"
	"testing"
	"time"
)

// discardingStream gives an account of its playback (playPosition) on its own
// clock, and at its first write discards audio, as a PCM that suspends does:
// with keep it had played the first half of that frame and gives back only
// the second half (takeAgain); without it, it gives back the whole frame, of
// which it played nothing. Its clock moves on 1 s across the suspend.
// replayAt is its clock when the Output wrote the replay.
type discardingStream struct {
	mu       sync.Mutex
	discard  bool
	keep     bool
	writes   int
	again    int
	now      int64
	delay    int
	first    int64
	replayAt int64
}

func (s *discardingStream) Write(frame []int16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	switch {
	case s.writes == 1:
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
	case s.writes == 2 && s.discard:
		s.replayAt = s.now
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
	return playPosition{running: true, delay: s.delay, before: s.now, after: s.now}, true
}

func (*discardingStream) Drop() (int, error) { return 0, nil }
func (*discardingStream) Close() error       { return nil }

// TestAnAudibleTimeComesFromTheWriteThatCarriesTheFirstSample fails if a
// speech whose first write the device partly discarded is timed by the
// account after the replay although the device had already played its first
// sample (GPT-6.1 Sol's review of #154 at 7d27574a): that sample's time is lost
// across the suspend, and the replay's account puts it a second late here. A
// late chime would shorten the time from the chime to the answer and could
// hide a leak. It fails too if a first sample the device discarded unplayed,
// which the replay writes again, is not timed by the account after that
// replay, or if the control, with no discard, is not timed at its write.
func TestAnAudibleTimeComesFromTheWriteThatCarriesTheFirstSample(t *testing.T) {
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
				first, replayAt := stream.first, stream.replayAt
				stream.mu.Unlock()
				switch {
				case tc.keep:
					t.Fatalf("the chime was timed at %d, though the device played its first sample before the discard, at %d, a time the replay's account cannot give: %+v", e.At, first, e)
				case tc.discard && e.At != replayAt:
					t.Fatalf("the replay carries the chime's first sample and its account puts it at %d; the event says %+v", replayAt, e)
				case !tc.discard && e.At != first:
					t.Fatalf("control: the chime's first sample left the buffer at %d; the event says %+v", first, e)
				}
			case <-time.After(300 * time.Millisecond):
				if !tc.keep {
					t.Fatal("no audible event for the chime")
				}
			}
		})
	}
}

// suspendingPCM is a fakePCM that, at the first write carrying the chime,
// takes half of it, plays 120 of those samples, and suspends with the other
// 120 still queued, for 150 ms: a resume that fails leaves the Output to
// replay the 120 and the half it never wrote (GPT-6.1 Sol's probe of #154).
type suspendingPCM struct {
	*fakePCM
	fired bool // only writei, on the mixer goroutine

	stampMu     sync.Mutex
	suspendedAt int64
	heardBefore int
}

func (p *suspendingPCM) writei(samples []int16, frames int) int {
	if p.fired || samples[0] <= 1000 {
		return p.fakePCM.writei(samples, frames)
	}
	p.fired = true
	n := p.fakePCM.writei(samples[:240], 240)
	if n <= 0 {
		return n
	}
	// Advance the device, not the process clock: 120 samples of the chime
	// played, and 120 stay queued when it suspends.
	p.mu.Lock()
	run := p.runs[len(p.runs)-1]
	target := len(run.samples) - 120
	p.clock += target - run.taken
	run.taken = target
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
	p.suspendedAt, p.heardBefore = at, prefix
	p.stampMu.Unlock()
	time.Sleep(150 * time.Millisecond)
	return n
}

// TestAnAudibleTimeIsNotGivenForASampleThePCMPlayedBeforeASuspend fails if,
// through the ALSA stream, a chime whose first 120 samples the PCM played
// before it suspended is still timed: the replay carries only the rest, and
// its account puts the first sample after the suspend (144 to 156 ms after it
// in Sol's probe of 7d27574a). No time is right for it, so the chime must have
// no audible event, and the acceptance driver falls back with a NOTE.
func TestAnAudibleTimeIsNotGivenForASampleThePCMPlayedBeforeASuspend(t *testing.T) {
	pcm := &suspendingPCM{fakePCM: newFakePCM(48000, 3840)}
	pcm.lockstep = true
	stream := newALSAStream(pcm, "suspending", alsaParams{rate: 48000, channels: 1, period: 480, buffer: 3840})
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
			t.Fatalf("the chime was timed at %d, %v after the PCM suspended, though it had played %d of its samples before: %+v", e.At, time.Duration(e.At-suspended), prefix, e)
		case <-deadline:
			pcm.stampMu.Lock()
			suspended, prefix := pcm.suspendedAt, pcm.heardBefore
			pcm.stampMu.Unlock()
			if suspended == 0 || prefix != 120 {
				t.Fatalf("fixture: suspended at %d, with %d of the chime's samples played before, want 120", suspended, prefix)
			}
			return
		}
	}
}
