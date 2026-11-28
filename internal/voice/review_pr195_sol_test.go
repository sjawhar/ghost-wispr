package voice

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Sol's review of #195 at 17bd4e47 left these probes; each failed there. The
// stop probe's late call is rejected, not logged: its first form required the
// Concierge to answer a call that came after the stop, which the review's own
// remedy forbids. The playback probe follows the rule of the review at
// 451690d6 (answerSpeechLocked).

// TestSol195ContinuousPlayback fails if an answer a driving voice writes into
// the speech its acknowledgement began, through that playback or a new one,
// is logged as answered on a stop, or if one it speaks in a speech of its own
// is not: voices B and C name no response in their audio, and audio that goes
// on in a speech begun before the answer came cannot be told from the
// acknowledgement's tail (TestSolFollowup195AckTailIsNotAnswer), so it proves
// no answer played. The control ends the acknowledgement's speech before the
// answer's begins.
func TestSol195ContinuousPlayback(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ackEnds, same bool
		want          QuestionOutcome
	}{
		{"a speech of its own control", true, false, OutcomeAnswered},
		{"a new playback in the acknowledgement's speech", false, false, OutcomeStopped},
		{"the acknowledgement's playback reused", false, true, OutcomeStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, sess, log := review195Driver(t, newFakeConcierge("ABC-12 is in review."))
			r.heard("what about ABC-12?", true, true)
			format := audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}
			pb := sess.x.Out.Begin()
			if err := pb.Write(t.Context(), make([]byte, 960), format); err != nil {
				t.Fatal(err)
			}
			r.speaker.FlushSpeech()
			if tc.ackEnds {
				r.speaker.endSpeech(audio.InputVoice)
				r.speaker.FlushSpeech()
			}
			review195Tool(t, sess, "what about ABC-12?")
			if !tc.same {
				pb = sess.x.Out.Begin()
			}
			if err := pb.Write(t.Context(), make([]byte, 960), format); err != nil {
				t.Fatal(err)
			}
			r.speaker.FlushSpeech()
			r.speaker.endSpeech(audio.InputVoice)
			r.speaker.FlushSpeech()
			r.s.Stop("Wispr, stop")
			q := log.next(t)
			if q.Outcome != tc.want || (q.FirstAudio != nil) != (tc.want == OutcomeAnswered) {
				t.Fatalf("logged %s, first audio %v; want %s, with a first audio only for an answer in a speech of its own", q.Outcome, q.FirstAudio, tc.want)
			}
		})
	}
}

// TestSol195ToolAfterStop fails if an answer tool call a driving voice makes
// after a stop reaches the Concierge or logs a question: a stop ends the
// exchange, and no turn of it is answered after. The control stops a call
// that is running, which is logged once, as stopped.
func TestSol195ToolAfterStop(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "stop while the call runs control"
		if late {
			name = "call after the stop"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r, sess, log := review195Driver(t, c)
			r.heard("what about ABC-12?", true, true)
			if late {
				r.s.Stop("Wispr, stop")
			}
			args, err := json.Marshal(AnswerArgs{Question: "what about ABC-12?"})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := sess.x.Tools.Run(context.Background(), ToolCall{Name: AnswerTool, Args: args})
				done <- err
			}()
			if late {
				select {
				case err := <-done:
					if !errors.Is(err, errExchangeEnded) {
						t.Errorf("the call after the stop returned %v, want the exchange's end", err)
					}
				case <-time.After(time.Second):
					t.Fatal("the call after the stop did not return")
				}
				select {
				case turn := <-c.asked:
					t.Errorf("the Concierge got %q after the stop", turn)
				default:
				}
				log.none(t, "a call after the stop", 100*time.Millisecond)
				return
			}
			review195WaitCall(t, c)
			r.s.Stop("Wispr, stop")
			if q := log.next(t); q.Outcome != OutcomeStopped {
				t.Fatalf("control: the stopped call was logged %s", q.Outcome)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("control: the stopped call returned no error")
				}
			case <-time.After(time.Second):
				t.Fatal("control: the stopped call did not return")
			}
			log.none(t, "a second row", 100*time.Millisecond)
		})
	}
}

// TestSol195ToolAfterNeverMind fails if an answer tool call a driving voice
// makes after "Wispr, never mind" logs any of the words the room cancelled.
func TestSol195ToolAfterNeverMind(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r, sess, log := review195Driver(t, c)
	r.heard("what about the cancelled-words-marker?", true, true)
	r.heard("Wispr, never mind.", true, true)
	args, err := json.Marshal(AnswerArgs{Question: "what about the cancelled-words-marker?"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = sess.x.Tools.Run(context.Background(), ToolCall{Name: AnswerTool, Args: args})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the call after never mind did not return")
	}
	r.s.Close()
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, q := range log.got {
		if q.Heard != "" || q.VendorWording != "" || q.Answer != "" || q.IssueKey != "" {
			t.Errorf("a call after never mind logged the cancelled words: %+v", q)
		}
	}
}

// sol195Concierge answers "what about ABC-12?" once first is closed, and
// holds any other turn until its context ends.
type sol195Concierge struct {
	*fakeConcierge
	first   chan struct{}
	entered chan string
}

func (c *sol195Concierge) Answer(ctx context.Context, x *Exchange, turn string, say SayFunc) error {
	c.entered <- turn
	if turn == "what about ABC-12?" {
		select {
		case <-c.first:
		case <-ctx.Done():
			return ctx.Err()
		}
		x.Outcome = OutcomeAnswered
		return say(ctx, "ABC-12 is in review.", Fact)
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestSol195OverlappingUnplayedAnswer fails if an answer that came back while
// a later answer tool call still runs is logged as answered before the voice
// played any of it: a stop before either plays logs both as stopped.
func TestSol195OverlappingUnplayedAnswer(t *testing.T) {
	base := newFakeConcierge()
	c := &sol195Concierge{fakeConcierge: base, first: make(chan struct{}), entered: make(chan string, 2)}
	log := &questionLog{ch: make(chan Question, 16)}
	r := newRigWith(t, base, func(cfg *SupervisorConfig, r *rig) {
		cfg.Concierge, cfg.Question = c, log.add
		cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
		cfg.IdleWait = time.Minute
	})
	r.heard("Wispr.", true, true)
	sess := r.waitSession()
	done := make(chan struct{}, 2)
	launch := func(question string) {
		args, err := json.Marshal(AnswerArgs{Question: question})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			_, _ = sess.x.Tools.Run(context.Background(), ToolCall{Name: AnswerTool, Args: args})
			done <- struct{}{}
		}()
		select {
		case <-c.entered:
		case <-time.After(time.Second):
			t.Fatal("the Concierge got no turn")
		}
	}
	r.heard("what about ABC-12?", true, true)
	launch("what about ABC-12?")
	r.heard("what about ABC-13?", true, true)
	launch("what about ABC-13?")
	close(c.first)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the first call did not return")
	}
	log.none(t, "the first answer before it played", 100*time.Millisecond)
	r.s.Stop("Wispr, stop")
	for range 2 {
		if q := log.next(t); q.Outcome != OutcomeStopped {
			t.Errorf("an answer the stop cut before it played, to %q, was logged %s", q.Heard, q.Outcome)
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the second call did not return")
	}
	log.none(t, "a third row", 100*time.Millisecond)
}
