package voice

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Real wall-clock delay, unchanged production horizon. Hold only callback
// delivery. Use a holder answer so no transcript-triggered FlushSpeech can
// deliver the callbacks before the later exchange ends on the #133 merge.
func TestReviewExpiredOwnerCallbacks(t *testing.T) {
	for _, afterLaterEnd := range []bool{false, true} {
		t.Run(fmt.Sprintf("deliver_after_later_end=%v", afterLaterEnd), func(t *testing.T) {
			var q *reviewSpeechQueue
			c := newFakeConcierge()
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				q = &reviewSpeechQueue{fakeSpeaker: r.speaker, holding: true, seen: make(chan audio.SpeechEvent, 16)}
				cfg.Speaker = q
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
				cfg.SilenceWait, cfg.SoundGap = time.Millisecond, time.Millisecond
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
			r.s.Stop("the completed speech's two callbacks still wait")
			eventually(t, "first exchange closed", func() bool { _, _, closed := first.state(); return closed })
			waitedFrom := time.Now()
			time.Sleep(ownWordsKept + 50*time.Millisecond)
			if !afterLaterEnd {
				q.release()
			}
			r.voice.failSays(errors.New("review: silent successor refuses Say"))
			c.answers <- PendingAnswer{TaskID: "later", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
			second := r.waitSession()
			eventually(t, "silent successor closed", func() bool { _, _, closed := second.state(); return closed })
			q.release()
			r.s.mu.Lock()
			reported := firstEx.notified[EventSpoken]
			r.s.mu.Unlock()
			t.Logf("callbacks waited %v; first exchange spoken=%v", time.Since(waitedFrom), reported)
			if !reported {
				t.Fatal("completed first speech lost when a later exchange pruned its owner before delivering its callbacks")
			}
			r.waitEvent("voice_spoken:sess-1")
		})
	}
}
