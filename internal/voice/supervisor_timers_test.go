package voice

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestATimerSeesASpeechWhoseStartIsQueued fails if a driving voice's idle
// wait or follow-up window, expiring just after its next speech began but
// before the speaker delivered that start, ends the exchange: the timer read
// the voice as quiet (Sol's review of #133 at 4238ad3). The timers' own
// callbacks are called, as their timers would. The control is a voice that
// stays quiet, whose exchange ends.
func TestATimerSeesASpeechWhoseStartIsQueued(t *testing.T) {
	for _, timer := range []string{"idle", "follow-up"} {
		for _, speaks := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, a speech begun %v", timer, speaks), func(t *testing.T) {
				r := newLongFollowUpDrivingRig(t)
				r.heard("Wispr, tell me the status?", true, true)
				sess := r.waitSession()
				if err := sess.x.Out.Begin().Write(t.Context(), []byte{1, 0}, ChimeFormat); err != nil {
					t.Fatal(err)
				}
				r.speaker.FlushSpeech()
				r.speaker.finish(audio.InputVoice)
				r.speaker.FlushSpeech()
				r.s.mu.Lock()
				ex := r.s.ex
				if ex == nil || ex.followUpTimer == nil || r.s.voiceSpeaking {
					r.s.mu.Unlock()
					t.Fatal("fixture: the follow-up window is not open")
				}
				idleGen, followUpGen := ex.idleGen, ex.followUpGen
				r.s.mu.Unlock()
				release := r.speaker.holdEvents()
				defer release()
				if speaks {
					if err := sess.x.Out.Begin().Write(t.Context(), []byte{1, 0}, ChimeFormat); err != nil {
						t.Fatal(err)
					}
					queuedStart(t, r, time.Time{})
				}
				if timer == "idle" {
					r.s.idleWaited(ex, idleGen)
				} else {
					r.s.driverFollowUpWaited(ex, followUpGen)
				}
				r.s.mu.Lock()
				ended := ex.ended || r.s.ex != ex
				r.s.mu.Unlock()
				if ended != !speaks {
					t.Fatalf("the exchange ended %v, want %v: a speech began %v", ended, !speaks, speaks)
				}
			})
		}
	}
}

// TestASupersededIdleWaitDoesNotEndTheExchange fails if an idle wait's
// callback, under way when the exchange was touched again, ends the exchange:
// a timer's Stop cannot recall a callback already running (Sol's review of
// #133 at 4238ad3). The callback waits for the Supervisor's lock, which the
// test holds while it touches the exchange. The control does not touch it,
// and the exchange ends.
func TestASupersededIdleWaitDoesNotEndTheExchange(t *testing.T) {
	for _, touched := range []bool{false, true} {
		t.Run(fmt.Sprintf("touched %v", touched), func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what is the status?", true, true)
			r.waitSession()
			entered, returned := make(chan struct{}), make(chan struct{})
			r.s.mu.Lock()
			ex := r.s.ex
			if ex == nil {
				r.s.mu.Unlock()
				t.Fatal("fixture: no exchange")
			}
			if ex.idle != nil {
				ex.idle.Stop()
			}
			gen := ex.idleGen
			ex.idle = time.AfterFunc(0, func() {
				close(entered)
				r.s.idleWaited(ex, gen)
				close(returned)
			})
			<-entered // the callback is under way
			if touched {
				r.s.touchLocked(ex)
			}
			r.s.mu.Unlock()
			select {
			case <-returned:
			case <-time.After(testWait):
				t.Fatal("the idle callback did not return")
			}
			r.s.mu.Lock()
			ended := ex.ended || r.s.ex != ex
			r.s.mu.Unlock()
			if ended == touched {
				t.Fatalf("the exchange ended %v, want %v: touched %v", ended, !touched, touched)
			}
		})
	}
}
