package voice

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// An AfterFunc context counts callbacks retained by a long-running meeting.
// Value hides the underlying cancelCtx, so context.AfterFunc uses this hook.
type reviewMeetingContext struct {
	context.Context
	mu     sync.Mutex
	active int
}

func (c *reviewMeetingContext) Value(any) any { return nil }
func (c *reviewMeetingContext) AfterFunc(fn func()) func() bool {
	c.mu.Lock()
	c.active++
	c.mu.Unlock()
	var once sync.Once
	remove := func() { once.Do(func() { c.mu.Lock(); c.active--; c.mu.Unlock() }) }
	stop := context.AfterFunc(c.Context, func() { remove(); fn() })
	return func() bool {
		stopped := stop()
		if stopped {
			remove()
		}
		return stopped
	}
}
func (c *reviewMeetingContext) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.active }

func TestReviewMeetingCallbacksReleased(t *testing.T) {
	for reason, path := range spokenEnds {
		if path.end == nil {
			continue
		}
		t.Run(reason, func(t *testing.T) {
			if path.limit > 0 {
				was := exchangeMax
				exchangeMax = path.limit
				t.Cleanup(func() { exchangeMax = was })
			}
			ctx, endMeeting := context.WithCancel(context.Background())
			defer endMeeting()
			meeting := &reviewMeetingContext{Context: ctx}
			c := newFakeConcierge("It ships tomorrow.")
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.Meeting = func(string) context.Context { return meeting }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
				if path.idle > 0 {
					cfg.IdleWait = path.idle
				}
				if path.followUp > 0 {
					cfg.FollowUpWait = path.followUp
				}
			})
			r.heard("Wispr, what is happening?", true, true)
			sess := r.waitSession()
			r.waitEvent("voice_exchange_started:sess-1")
			if n := meeting.count(); n != 2 {
				t.Fatalf("control: %d live meeting callbacks, want 2", n)
			}
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.speaker.finish(audio.InputVoice)
			r.waitEvent("voice_spoken:sess-1")
			if path.meeting {
				endMeeting()
			}
			path.end(t, r, c, sess)
			eventually(t, "the first exchange closed", func() bool { _, _, closed := sess.state(); return closed })
			// A holder's answer starts an exchange too; shutdown checks its
			// cleanup without ending the meeting that retains the callbacks.
			r.s.Close()
			eventually(t, "meeting callbacks removed", func() bool { return meeting.count() == 0 })
		})
	}
}
