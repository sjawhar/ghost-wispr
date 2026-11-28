package voice

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Sol's review of #195 at 75c3253 left these probes. The oldest-answer probe
// failed there; the deferred-end probe passed, and stays as a regression
// test, with the outcome its quiet end now has (OutcomeUnconfirmed).

// solFinal195CounterSession is a session that reports queued events
// (EventCounter), so a test can leave a speech's judgment waiting.
type solFinal195CounterSession struct {
	Session
	queued uint64
}

func (s solFinal195CounterSession) EventsQueued() uint64 { return s.queued }

// TestSolFinal195DeferredEndDoesNotWait fails if a stop, the follow-up
// window's end or the voice's close waits for a speech's judgment whose
// event never comes, logs the turn other than once, or leaves the next
// exchange the old counters or judgment. The control's events are all read.
func TestSolFinal195DeferredEndDoesNotWait(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, end := range []string{"stop", "follow-up end", "voice close"} {
			name := end + "/read control"
			if deferred {
				name = end + "/queued event never forwarded"
			}
			t.Run(name, func(t *testing.T) {
				r, sess, log := review195Driver(t, newFakeConcierge("ABC-12 is in review."))
				r.heard("what about ABC-12?", true, true)
				ack := sess.x.Out.Begin()
				solFollowupFrame(t, ack)
				r.speaker.FlushSpeech()
				review195Tool(t, sess, "what about ABC-12?")
				r.s.mu.Lock()
				ex := r.s.ex
				queued := ex.eventsRead
				if deferred {
					queued++
				}
				ex.session = solFinal195CounterSession{Session: ex.session, queued: queued}
				r.s.mu.Unlock()
				r.speaker.endSpeech(audio.InputVoice)
				r.speaker.FlushSpeech()
				r.s.mu.Lock()
				pending := len(ex.spoken)
				r.s.mu.Unlock()
				if (pending > 0) != deferred {
					t.Fatalf("control: %d speeches await judgment, want them only when deferred", pending)
				}
				finished := make(chan struct{})
				go func() {
					switch end {
					case "stop":
						r.s.Stop("Wispr, stop")
					case "follow-up end":
						r.s.mu.Lock()
						gen := ex.followUpGen
						r.s.mu.Unlock()
						r.s.driverFollowUpWaited(ex, gen)
					case "voice close":
						_ = sess.Close()
					}
					close(finished)
				}()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Fatal("the end waited for an event never forwarded")
				}
				want := map[string]QuestionOutcome{"stop": OutcomeStopped, "follow-up end": OutcomeUnconfirmed, "voice close": OutcomeFailed}[end]
				if q := log.next(t); q.Outcome != want {
					t.Fatalf("logged %s, want %s", q.Outcome, want)
				}
				log.none(t, "a second row", 50*time.Millisecond)
				r.heard("Wispr.", true, true)
				next := r.waitSession()
				r.s.mu.Lock()
				nextEx := r.s.ex
				inherited := nextEx == nil || nextEx == ex || nextEx.eventsRead != 0 || len(nextEx.spoken) != 0
				r.s.mu.Unlock()
				if inherited {
					t.Fatal("the next exchange inherited the old counters or judgment")
				}
				r.heard("what about ABC-13?", true, true)
				review195Tool(t, next, "what about ABC-13?")
				solFollowupFrame(t, next.x.Out.Begin())
				r.speaker.FlushSpeech()
				r.speaker.endSpeech(audio.InputVoice)
				r.speaker.FlushSpeech()
				if q := log.next(t); q.Outcome != OutcomeAnswered {
					t.Fatalf("the next exchange's turn logged %s, want answered", q.Outcome)
				}
				r.s.Stop("Wispr, stop")
				log.none(t, "a second row of the next exchange", 50*time.Millisecond)
			})
		}
	}
}

// solFinal195OrderConcierge answers each turn once the test releases it.
type solFinal195OrderConcierge struct {
	*fakeConcierge
	entered  chan string
	releases map[string]chan struct{}
}

func (c *solFinal195OrderConcierge) Answer(ctx context.Context, x *Exchange, turn string, say SayFunc) error {
	x.Asked = turn
	c.entered <- turn
	select {
	case <-c.releases[turn]:
	case <-ctx.Done():
		return ctx.Err()
	}
	return say(ctx, "The answer to "+turn, Fact)
}

// TestSolFinal195OldestReturnedAnswerFirst fails if a speech begun after two
// answers came is taken to play the answer to the question asked first
// rather than the answer that came first. The control's answers come in the
// order their questions were asked.
func TestSolFinal195OldestReturnedAnswerFirst(t *testing.T) {
	const first, second = "what about ABC-12?", "what about ABC-13?"
	for _, reverse := range []bool{false, true} {
		name := "answers in question order control"
		if reverse {
			name = "the second question's answer comes first"
		}
		t.Run(name, func(t *testing.T) {
			base := newFakeConcierge()
			c := &solFinal195OrderConcierge{fakeConcierge: base, entered: make(chan string, 2), releases: map[string]chan struct{}{first: make(chan struct{}), second: make(chan struct{})}}
			log := &questionLog{ch: make(chan Question, 16)}
			r := newRigWith(t, base, func(cfg *SupervisorConfig, r *rig) {
				cfg.Concierge, cfg.Question = c, log.add
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait = time.Minute
			})
			r.heard("Wispr.", true, true)
			sess := r.waitSession()
			launch := func(turn string) <-chan error {
				r.heard(turn, true, true)
				args, err := json.Marshal(AnswerArgs{Question: turn})
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					_, err := sess.x.Tools.Run(t.Context(), ToolCall{Name: AnswerTool, Args: args})
					done <- err
				}()
				select {
				case <-c.entered:
				case <-time.After(time.Second):
					t.Fatal("the Concierge got no turn")
				}
				return done
			}
			d1, d2 := launch(first), launch(second)
			a, b, da, db := first, second, d1, d2
			if reverse {
				a, b, da, db = second, first, d2, d1
			}
			close(c.releases[a])
			if err := <-da; err != nil {
				t.Fatal(err)
			}
			close(c.releases[b])
			if err := <-db; err != nil {
				t.Fatal(err)
			}
			solFollowupFrame(t, sess.x.Out.Begin())
			r.speaker.FlushSpeech()
			r.speaker.endSpeech(audio.InputVoice)
			r.speaker.FlushSpeech()
			if q := log.next(t); q.Heard != a || q.Outcome != OutcomeAnswered {
				t.Errorf("the speech played %q as %s, want the answer that came first, to %q", q.Heard, q.Outcome, a)
			}
			r.s.Stop("Wispr, stop")
			if q := log.next(t); q.Heard != b || q.Outcome != OutcomeStopped {
				t.Errorf("the unplayed answer: %+v, want %q stopped", q, b)
			}
			log.none(t, "a third row", 50*time.Millisecond)
		})
	}
}
