package voice

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Sol's review of #195 at 14fd94eb left these probes. The late-report probe
// failed there; the blocked-judgment probe passed, and stays as a regression
// test of every way an exchange can end while a tool waits for judgment.

// TestSolRecheck195AudioBeforeAnswerReportedLate fails if a speech of the
// voice that began and ended before the answer came, whose start and end the
// speaker reported only after it, is taken for audio after the answer: the
// voice played nothing after it, so the idle wait logs the answer as failed
// and plays the error sound. A person's words close the follow-up window, so
// the idle wait decides. The controls report the speech before the answer,
// or only its end after it; the policy case's audio goes on past the
// answer, which may have been the answer, and gets no sound.
func TestSolRecheck195AudioBeforeAnswerReportedLate(t *testing.T) {
	for _, phase := range []string{"all reported before the answer control", "only the end reported late control", "start and end reported late", "the acknowledgement goes on past the answer"} {
		t.Run(phase, func(t *testing.T) {
			r, sess, log := review195Driver(t, newFakeConcierge("ABC-12 is in review."))
			r.heard("what about ABC-12?", true, true)
			release := r.speaker.holdEvents()
			defer release()
			ack := sess.x.Out.Begin()
			solFollowupFrame(t, ack)
			if phase != "start and end reported late" {
				r.speaker.FlushSpeech()
			}
			goesOn := phase == "the acknowledgement goes on past the answer"
			if !goesOn {
				r.speaker.endSpeech(audio.InputVoice)
			}
			if phase == "all reported before the answer control" {
				r.speaker.FlushSpeech()
			}
			review195Tool(t, sess, "what about ABC-12?")
			if goesOn {
				solFollowupFrame(t, ack)
				r.speaker.endSpeech(audio.InputVoice)
			}
			r.speaker.FlushSpeech()
			r.heard("okay so the budget looks fine", true, true)
			r.s.mu.Lock()
			ex := r.s.ex
			gen := ex.idleGen
			r.s.mu.Unlock()
			r.s.idleWaited(ex, gen)
			want := OutcomeFailed
			if goesOn {
				want = OutcomeUnconfirmed
			}
			if q := log.next(t); q.Outcome != want {
				t.Errorf("logged %s, want %s", q.Outcome, want)
			}
			log.none(t, "a second row", 30*time.Millisecond)
			if !goesOn {
				eventually(t, "the error sound", func() bool {
					return slices.Contains(r.speaker.soundsPlayed(), "error")
				})
			} else if slices.Contains(r.speaker.soundsPlayed(), "error") {
				t.Error("audio that went on past the answer got the error sound")
			}
		})
	}
}

// TestSolRecheck195BlockedJudgmentToolEnds fails if an answer tool call that
// waits for a speech's judgment whose event never comes outlives any end of
// its exchange, returns no error, or leaves the exchange's answered question
// logged other than once, by how the exchange ended. The idle wait's and the
// follow-up window's guards, which a running call holds off, must not
// release it.
func TestSolRecheck195BlockedJudgmentToolEnds(t *testing.T) {
	for _, ending := range []string{"stop", "never mind", "thanks", "sessions deleted", "hands up", "gate", "shutdown", "voice close", "voice error", "meeting end", "exchange cap", "caller cancel", "idle guard", "follow-up guard"} {
		t.Run(ending, func(t *testing.T) {
			if ending == "exchange cap" {
				was := exchangeMax
				exchangeMax = 400 * time.Millisecond
				t.Cleanup(func() { exchangeMax = was })
			}
			meeting, endMeeting := context.WithCancel(t.Context())
			defer endMeeting()
			log := &questionLog{ch: make(chan Question, 16)}
			r := newRigWith(t, newFakeConcierge("ABC-12 is in review."), func(cfg *SupervisorConfig, r *rig) {
				cfg.Question = log.add
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.Meeting = func(string) context.Context { return meeting }
				cfg.IdleWait = time.Minute
			})
			r.heard("Wispr.", true, true)
			sess := r.waitSession()
			r.heard("what about ABC-12?", true, true)
			ack := sess.x.Out.Begin()
			solFollowupFrame(t, ack)
			r.speaker.FlushSpeech()
			review195Tool(t, sess, "what about ABC-12?")
			r.s.mu.Lock()
			ex := r.s.ex
			ex.session = solFinal195CounterSession{Session: ex.session, queued: ex.eventsRead + 1}
			r.s.mu.Unlock()
			r.speaker.endSpeech(audio.InputVoice)
			r.speaker.FlushSpeech()
			r.heard("what about ABC-13?", true, true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			args, err := json.Marshal(AnswerArgs{Question: "what about ABC-13?"})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := sess.x.Tools.Run(ctx, ToolCall{Name: AnswerTool, Args: args})
				done <- err
			}()
			eventually(t, "the call waiting for the judgment", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return ex.toolCalls == 1 && len(ex.spoken) == 1
			})
			select {
			case err := <-done:
				t.Fatalf("control: the call did not wait: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			want := OutcomeStopped
			switch ending {
			case "stop":
				r.s.Stop("Wispr, stop")
			case "never mind":
				r.heard("Wispr, never mind.", true, true)
				want = OutcomeNeverMind
			case "thanks":
				r.heard("Thanks, Wispr.", true, true)
			case "sessions deleted":
				r.s.SessionsDeleted([]string{"sess-1"})
			case "hands up":
				r.heard("Wispr, who has a hand up?", true, true)
			case "gate":
				r.closeGate()
			case "shutdown":
				r.s.Close()
			case "voice close":
				_ = sess.Close()
				want = OutcomeFailed
			case "voice error":
				sess.fail(errors.New("the vendor failed"))
				want = OutcomeFailed
			case "meeting end":
				endMeeting()
			case "exchange cap":
				// The exchange's deadline ends it (capExchange).
			case "caller cancel":
				cancel()
			case "idle guard", "follow-up guard":
				r.s.mu.Lock()
				idle, follow := ex.idleGen, ex.followUpGen
				r.s.mu.Unlock()
				if ending == "idle guard" {
					r.s.idleWaited(ex, idle)
				} else {
					r.s.driverFollowUpWaited(ex, follow)
				}
				select {
				case err := <-done:
					t.Fatalf("the guard released the waiting call: %v", err)
				case <-time.After(25 * time.Millisecond):
				}
				r.s.Stop("Wispr, stop")
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("the waiting call returned no error")
				}
			case <-time.After(time.Second):
				t.Fatal("the waiting call outlived the exchange's end")
			}
			if ending == "caller cancel" {
				r.s.Stop("Wispr, stop")
			}
			if q := log.next(t); q.Outcome != want {
				t.Errorf("the answered question logged %s, want %s", q.Outcome, want)
			}
			log.none(t, "a second row, or one for the waiting call", 30*time.Millisecond)
		})
	}
}
