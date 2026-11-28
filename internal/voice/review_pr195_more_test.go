package voice

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Sol's review of #195 at 48ca91a1 left these probes, for a voice that
// drives its own turn; each failed there.

// review195Driver opens a driving voice's exchange on a bare call.
func review195Driver(t *testing.T, c *fakeConcierge) (*rig, *fakeSession, *questionLog) {
	t.Helper()
	r, log := newQuestionRig(t, c, true)
	r.heard("Wispr.", true, true)
	sess := r.waitSession()
	return r, sess, log
}

// review195Tool runs the answer tool for question, as the vendor does.
func review195Tool(t *testing.T, sess *fakeSession, question string) {
	t.Helper()
	args, err := json.Marshal(AnswerArgs{Question: question})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.x.Tools.Run(t.Context(), ToolCall{Name: AnswerTool, Args: args}); err != nil {
		t.Fatal(err)
	}
}

// review195WaitCall waits until the Concierge got a turn.
func review195WaitCall(t *testing.T, c *fakeConcierge) {
	t.Helper()
	select {
	case <-c.asked:
	case <-time.After(time.Second):
		t.Fatal("the Concierge got no turn")
	}
}

// TestReview195ClaimKeepsItsCaptureSpan fails if a claimed request
// (Exchange.Claim) is logged with the capture span of a later final that
// came before the answer tool ran: the row's words are the claim's, and so
// must be its times. The control has no later final.
func TestReview195ClaimKeepsItsCaptureSpan(t *testing.T) {
	for _, later := range []bool{false, true} {
		name := "no later final control"
		if later {
			name = "later final before the tool"
		}
		t.Run(name, func(t *testing.T) {
			r, sess, log := review195Driver(t, newFakeConcierge())
			first := time.Now()
			tr := transcriptOf("what about ABC-12?", 1, true, true, 0)
			tr.At = first
			r.s.OnTranscript(&tr)
			view := sess.x.Room()
			if !sess.x.Claim(view.Turn, view.Revision) {
				t.Fatal("control: the first request was not claimed")
			}
			if later {
				tr = transcriptOf("what about ABC-13?", 20, true, true, 0)
				tr.At = first.Add(20 * time.Second)
				r.s.OnTranscript(&tr)
			}
			review195Tool(t, sess, "what about ABC-12?")
			q := log.next(t)
			if q.Heard != "what about ABC-12?" {
				t.Fatalf("control: logged the wrong request: %+v", q)
			}
			if q.HeardFrom.Sub(first) > time.Second {
				t.Fatalf("the claimed words were logged with the later final's span: said %s, heard from %s", first, q.HeardFrom)
			}
		})
	}
}

// TestReview195AnAnswerInItsAcknowledgementsSpeechIsUnproven fails if an
// answer a driving voice speaks in a speech of its own is logged as anything
// but answered, or if one it speaks in the speech its acknowledgement began
// is logged as answered on a stop: no audio of that speech tells the answer
// from the acknowledgement's tail (answerSpeechLocked). This probe of Sol's
// review at 48ca91a1 first required answered for both; the review at
// 451690d6 showed the acknowledgement's tail would then pass for an answer.
func TestReview195AnAnswerInItsAcknowledgementsSpeechIsUnproven(t *testing.T) {
	for _, early := range []bool{false, true} {
		name := "a speech of its own control"
		want := OutcomeAnswered
		if early {
			name, want = "the answer continues the acknowledgement", OutcomeStopped
		}
		t.Run(name, func(t *testing.T) {
			r, sess, log := review195Driver(t, newFakeConcierge("ABC-12 is in review."))
			r.heard("what about ABC-12?", true, true)
			if early {
				speakAs(t, sess, "Checking.")
				r.speaker.FlushSpeech()
			}
			review195Tool(t, sess, "what about ABC-12?")
			speakAs(t, sess, "ABC-12 is in review.")
			r.speaker.FlushSpeech()
			r.speaker.endSpeech(audio.InputVoice)
			r.speaker.FlushSpeech()
			r.s.Stop("Wispr, stop")
			if q := log.next(t); q.Outcome != want {
				t.Fatalf("logged %+v, want %s", q, want)
			}
		})
	}
}

// TestReview195EveryInflightQuestionGetsTheStop fails if a stop leaves an
// answer tool call running, or logs an answer still being made other than as
// stopped, one row each, when one or two run: a stop ends the Concierge's
// calls at once, whatever context the vendor's call came with.
func TestReview195EveryInflightQuestionGetsTheStop(t *testing.T) {
	for _, two := range []bool{false, true} {
		name := "one in flight control"
		if two {
			name = "two in flight"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			c.hold, c.outcome = true, OutcomeAnswered
			r, sess, log := review195Driver(t, c)
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
			}
			r.heard("what about ABC-12?", true, true)
			launch("what about ABC-12?")
			review195WaitCall(t, c)
			n := 1
			if two {
				r.heard("what about ABC-13?", true, true)
				launch("what about ABC-13?")
				review195WaitCall(t, c)
				n = 2
			}
			r.s.Stop("Wispr, stop")
			for range n {
				if q := log.next(t); q.Outcome != OutcomeStopped {
					t.Errorf("an answer the stop cut was logged %+v", q)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("the answer tool did not end with the stop")
				}
			}
			log.none(t, "a row beyond one for each call", 100*time.Millisecond)
		})
	}
}
