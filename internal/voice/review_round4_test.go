package voice

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Keep real speaker callback order, delaying delivery but never playback.
// Compare release while the second exchange holds the floor with release
// immediately after the same silent second exchange has ended.
func TestReviewDelayedStartAcrossTwoEndedExchanges(t *testing.T) {
	for _, endSecond := range []bool{false, true} {
		t.Run(fmt.Sprintf("second_ended=%v", endSecond), func(t *testing.T) {
			var q *reviewSpeechQueue
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				q = &reviewSpeechQueue{fakeSpeaker: r.speaker, holding: true, seen: make(chan audio.SpeechEvent, 16)}
				cfg.Speaker = q
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
			})
			t.Cleanup(q.release)
			r.heard("Wispr, what is happening?", true, true)
			first := r.waitSession()
			r.waitEvent("voice_exchange_started:sess-1")
			r.s.mu.Lock()
			firstEx := r.s.ex
			r.s.mu.Unlock()
			first.mu.Lock()
			first.endLate = time.Second
			first.mu.Unlock()
			speakAs(t, first, "The issue is in review since this morning.")
			q.wait(t, true)
			r.speaker.finish(audio.InputVoice)
			q.wait(t, false)
			r.s.Stop("first completed exchange ends before callback delivery")
			eventually(t, "first closed", func() bool { _, _, closed := first.state(); return closed })
			r.heard("Wispr, what happens next?", true, true)
			second := r.waitSession()
			r.waitEvent("voice_exchange_started:sess-1")
			r.s.mu.Lock()
			secondEx := r.s.ex
			r.s.mu.Unlock()
			if secondEx == nil || secondEx == firstEx {
				t.Fatal("control: no distinct second exchange")
			}
			if endSecond {
				r.s.Stop("silent second exchange ends before callback delivery")
				eventually(t, "second closed", func() bool { _, _, closed := second.state(); return closed })
			}
			q.release()
			r.s.mu.Lock()
			firstSpoken, secondSpoken := firstEx.notified[EventSpoken], secondEx.notified[EventSpoken]
			r.s.mu.Unlock()
			if !firstSpoken || secondSpoken {
				t.Fatalf("completed first exchange spoken=%v, silent second spoken=%v; want true,false", firstSpoken, secondSpoken)
			}
			r.waitEvent("voice_spoken:sess-1")
		})
	}
}
