package voice

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// nopPlayback queues nothing and allocates nothing, so a count of
// allocations around its Write is the Supervisor's own.
type nopPlayback struct{}

func (nopPlayback) Write(context.Context, []byte, audio.AudioFormat) error { return nil }
func (nopPlayback) Drain(context.Context) error                            { return nil }

// nopVoiceSpeaker is the fake speaker with every voice playback a nopPlayback.
type nopVoiceSpeaker struct{ *fakeSpeaker }

func (s nopVoiceSpeaker) Begin(input string) Playback {
	if input == audio.InputVoice {
		return nopPlayback{}
	}
	return s.fakeSpeaker.Begin(input)
}

// TestAnExchangeFrameAllocatesNothing fails if writing one frame of an
// exchange's speech allocates, past the playback's first frame: a goroutine
// that allocates on the audio path is drafted into the garbage collector's
// assist, which stalls the speaker into underruns. It holds for an exchange
// bound to its meeting, and for a hand's words, whose authority also reads
// the membership; the voice writes every frame of a Say under one context.
func TestAnExchangeFrameAllocatesNothing(t *testing.T) {
	for _, hand := range []bool{false, true} {
		t.Run(map[bool]string{false: "an exchange", true: "a hand's words"}[hand], func(t *testing.T) {
			meeting, end := context.WithCancel(context.Background())
			defer end()
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				cfg.Meeting = func(string) context.Context { return meeting }
				cfg.Speaker = nopVoiceSpeaker{r.speaker}
			})
			r.heard("Wispr, tell me.", false, false)
			session := r.waitSession()
			if hand {
				membership, endMembership := context.WithCancel(context.Background())
				defer endMembership()
				r.s.mu.Lock()
				ex := r.s.ex
				joined, stop := joinContexts(ex.ctx, membership)
				defer stop()
				ex.sayCtx = joined
				r.s.mu.Unlock()
			}
			pb := session.x.Out.Begin()
			caller, cancel := context.WithCancel(context.Background())
			defer cancel()
			frame := make([]byte, 960)
			if err := pb.Write(caller, frame, ChimeFormat); err != nil {
				t.Fatalf("control: a live frame was refused: %v", err)
			}
			allocs := testing.AllocsPerRun(200, func() {
				if err := pb.Write(caller, frame, ChimeFormat); err != nil {
					panic(err)
				}
			})
			if allocs != 0 {
				t.Fatalf("each frame written allocates %.1f times, want none", allocs)
			}
		})
	}
}

// TestASoundFrameCheckAllocatesNothing fails if the check a notification
// sound's frame makes of its owners (soundContext.Err, which the speaker's
// Write reads at each frame) allocates.
func TestASoundFrameCheckAllocatesNothing(t *testing.T) {
	meeting, end := context.WithCancel(context.Background())
	defer end()
	membership, endMembership := context.WithCancel(context.Background())
	defer endMembership()
	h := testHand("h1", "ABC-12", time.Minute)
	h.Context = membership
	ctx := newSoundContext(context.Background(), meeting, []*pendingAnswer{{hand: h}})
	defer ctx.cancel()
	allocs := testing.AllocsPerRun(200, func() {
		if err := ctx.Err(); err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("each frame's check of the sound's owners allocates %.1f times, want none", allocs)
	}
}
