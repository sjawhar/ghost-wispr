package voice

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// endedExchange makes an exchange whose context ends when its cancel is
// called, holding the floor of r's Supervisor, as a meeting's end leaves one
// before the meeting's callback runs.
func endedExchange(r *rig) (*exchange, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ex := &exchange{ctx: ctx, cancel: cancel, x: Exchange{SessionID: "sess-1"}, notified: map[string]bool{}}
	r.s.mu.Lock()
	r.s.takeFloorLocked(ex)
	r.s.mu.Unlock()
	return ex, cancel
}

// TestAnExchangeEndedByItsContextCutsTheVoice (Sol's review of #184 at
// 5d74a100): a meeting's end cancels the exchange's context, which wakes the
// answer's wait for its speech, and finish can take the lock before the
// meeting's callback. It fails if finish then retires the exchange without
// cutting the voice input, as it did ("done"), or plays the error sound: the
// samples the speaker holds would play on after the meeting ended. The same
// holds for every other way an exchange is retired once its context ended.
func TestAnExchangeEndedByItsContextCutsTheVoice(t *testing.T) {
	for name, tc := range map[string]struct {
		// speaks: the exchange's voice has speech queued; an idle wait ends
		// only an exchange whose voice is quiet.
		speaks bool
		end    func(r *rig, ex *exchange)
	}{
		"finish": {true, func(r *rig, ex *exchange) {
			if r.s.finish(ex, "sess-1") {
				t.Error("a cancelled speech was reported spoken")
			}
		}},
		"the follow-up window's end": {true, func(r *rig, ex *exchange) {
			r.s.mu.Lock()
			ex.followUp = true
			r.s.mu.Unlock()
			r.s.turnWaited(ex)
		}},
		"an idle driving voice": {false, func(r *rig, ex *exchange) {
			r.s.mu.Lock()
			ex.drives = true
			gen := ex.idleGen
			r.s.mu.Unlock()
			r.s.idleWaited(ex, gen)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, newFakeConcierge())
			ex, cancel := endedExchange(r)
			if tc.speaks {
				r.speaker.holdDrain(audio.InputVoice)
				pb := r.speaker.Begin(audio.InputVoice, ex)
				if err := pb.Write(ex.ctx, make([]byte, 960), ChimeFormat); err != nil {
					t.Fatal(err)
				}
				r.s.mu.Lock()
				ex.last, ex.said = pb, true
				r.s.mu.Unlock()
			}
			before, _ := r.speaker.cancelledAt(audio.InputVoice)
			cancel()
			tc.end(r, ex)
			r.s.mu.Lock()
			ended := ex.ended
			r.s.mu.Unlock()
			if !ended {
				t.Fatal("the exchange was not retired")
			}
			if after, _ := r.speaker.cancelledAt(audio.InputVoice); !after.After(before) {
				t.Error("the exchange was retired with its context ended and its voice input not cut")
			}
			for _, event := range r.timingEvents() {
				if event == "error_sound" {
					t.Error("a context's end played the error sound")
				}
			}
		})
	}
}

// countingStream is a 24 kHz speaker stream that plays each 10 ms frame in
// about real time and counts the samples it played that are not silence.
type countingStream struct {
	mu     sync.Mutex
	voiced int
}

func (s *countingStream) Write(p []int16) error {
	s.mu.Lock()
	for _, v := range p {
		if v != 0 {
			s.voiced++
		}
	}
	s.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	return nil
}

func (*countingStream) Drop() (int, error) { return 0, nil }
func (*countingStream) Close() error       { return nil }

func (s *countingStream) played() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.voiced
}

// TestAMeetingEndDiscardsTheSpeechTheSpeakerHolds (Sol's review of #184 at
// 5d74a100) uses the real speaker output: speech is written ahead of what
// plays, up to its queue, so once the exchange's context ends, a refused
// write stops no sample already queued. It fails if more than one 10 ms
// frame of the speech plays after finish retires the exchange whose context
// a meeting's end cancelled; the control, retired with its context live,
// plays its queue out.
func TestAMeetingEndDiscardsTheSpeechTheSpeakerHolds(t *testing.T) {
	const rate = 24000
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "control: the context is live", true: "the meeting ended"}[ended], func(t *testing.T) {
			stream := &countingStream{}
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) { return stream, rate, nil }})
			t.Cleanup(func() { _ = out.Close() })
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, _ *rig) { cfg.Speaker = SpeakerOf(out) })
			ex, cancel := endedExchange(r)
			defer cancel()
			pb := r.s.cfg.Speaker.Begin(audio.InputVoice, ex)
			frame := make([]byte, 2*rate/50) // 20 ms
			for i := 0; i < len(frame); i += 2 {
				binary.LittleEndian.PutUint16(frame[i:], 4000)
			}
			// 1 s of speech; Write waits while the speaker's queue is full.
			written := make(chan struct{})
			go func() {
				defer close(written)
				for range 50 {
					if pb.Write(ex.ctx, frame, audio.AudioFormat{SampleRate: rate, Channels: 1, Encoding: audio.EncodingPCM16}) != nil {
						return
					}
				}
			}()
			deadline := time.Now().Add(2 * time.Second)
			for stream.played() < rate*3/10 && time.Now().Before(deadline) { // 300 ms played
				time.Sleep(5 * time.Millisecond)
			}
			r.s.mu.Lock()
			ex.last, ex.said = pb, true
			r.s.mu.Unlock()
			if ended {
				cancel()
				r.s.finish(ex, "sess-1")
			} else {
				r.s.mu.Lock()
				r.s.endLocked(ex, false, "done")
				r.s.mu.Unlock()
			}
			at := stream.played()
			time.Sleep(500 * time.Millisecond)
			after := stream.played() - at
			if ended && after > rate/100 {
				t.Errorf("%d samples (%v) of the speech played after the meeting ended, want at most one 10 ms frame", after, time.Duration(after)*time.Second/rate)
			}
			if !ended && after < rate/10 {
				t.Errorf("control: %d samples played after a retirement with a live context, want the queue played out", after)
			}
			<-written
		})
	}
}
