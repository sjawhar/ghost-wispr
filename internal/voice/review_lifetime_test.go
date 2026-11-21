package voice

import (
	"fmt"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// reviewQueueEndedOwner plays a short speech owned by a new exchange on out,
// marks the exchange ended without cancelling the input, as a quiet end
// does, and returns a weak pointer to it: returning from its own frame drops
// the caller's last strong reference.
//
//go:noinline
func reviewQueueEndedOwner(t *testing.T, out *audio.Output) weak.Pointer[exchange] {
	t.Helper()
	ex := &exchange{x: Exchange{SessionID: "review-ended-owner"}}
	w := weak.Make(ex)
	pb := out.Begin(audio.InputVoice, ex)
	if err := pb.Write(t.Context(), make([]byte, 960), audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err != nil {
		t.Fatal(err)
	}
	if err := pb.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	ex.ended = true // a quiet exchange end does not Cancel the speaker
	return w
}

// TestReviewRound8OwnerLifetimeAfterIdle is Sol's round-8 probe of #143: an
// exchange whose speech ended, and whose Output then closed its stream for
// being idle, must be collectable, though no Cancel came. The Output kept
// the input's last playback, and through its owner the exchange, for as long
// as it lived; closeIdle now lets go of an idle input's playbacks. The
// control cancels the input after the idle close, as Sol's probe did; since
// #148 the Output also records whose samples each recent stream position
// held (heldBy), which a Cancel does not clear, so the idle close must clear
// them in both cases.
func TestReviewRound8OwnerLifetimeAfterIdle(t *testing.T) {
	for _, cancel := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel_after_idle=%v", cancel), func(t *testing.T) {
			closed := make(chan struct{})
			out := audio.NewOutput(audio.OutputConfig{
				IdleClose: 20 * time.Millisecond,
				Open:      func(string) (audio.PlaybackStream, int, error) { return &reviewPacedDevice{}, 48000, nil },
				Closing:   func(closeStream func() error) { _ = closeStream(); close(closed) },
			})
			defer func() { _ = out.Close() }()
			ended := make(chan struct{}, 1)
			remove := out.OnSpeech(func(e audio.SpeechEvent) {
				if !e.Started {
					ended <- struct{}{}
				}
			})
			defer remove()
			w := reviewQueueEndedOwner(t, out)
			waitFor(t, ended, "speech end delivered")
			waitFor(t, closed, "idle stream closed")
			if cancel {
				out.Cancel(audio.InputVoice)
			}
			// Collected within two seconds: a goroutine still delivering the
			// end event, on a loaded machine, can hold it until then.
			alive := true
			for deadline := time.Now().Add(2 * time.Second); alive && time.Now().Before(deadline); {
				runtime.GC()
				if alive = w.Value() != nil; alive {
					time.Sleep(10 * time.Millisecond)
				}
			}
			runtime.KeepAlive(out)
			if alive {
				t.Fatal("Output still retains the ended exchange two seconds after both events were delivered and the stream closed idle")
			}
		})
	}
}
