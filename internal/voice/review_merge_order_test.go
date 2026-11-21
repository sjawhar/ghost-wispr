package voice

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestReviewMergedCompletedSpeechBeforeStop is Sol's round-2 probe of #143,
// run on the tree merged with #133, kept as a test: a driving voice's speech
// that played to its end, whose end report is held past a stop or delivered
// before it, must be reported spoken. The probe held the speaker's reports
// with #133's holdEvents and FlushSpeech; heldReports holds them here.
func TestReviewMergedCompletedSpeechBeforeStop(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(fmt.Sprintf("delayed=%v", delayed), func(t *testing.T) {
			var reports *heldReports
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				reports = holdReports(t, cfg, r)
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
			})
			r.heard("Wispr, what is happening?", true, true)
			sess := r.waitSession()
			sess.mu.Lock()
			sess.endLate = time.Second
			sess.mu.Unlock()
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.speaker.finish(audio.InputVoice)
			waitFor(t, reports.held, "the speech's end report")
			if !delayed {
				reports.release()
			}
			r.s.Stop("review stop")
			eventually(t, "exchange closed", func() bool { _, _, closed := sess.state(); return closed })
			reports.release()
			r.waitEvent("voice_spoken:sess-1")
		})
	}
}
