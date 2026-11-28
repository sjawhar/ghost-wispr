package voice

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Sol's review of #195 at 451690d6 left these probes; each failed there.
// Voices B and C name no response in their audio, so the Supervisor counts an
// answer played only when a speech the voice began after the answer came ends
// by itself, one answer to a speech, the oldest first; audio that continues a
// speech begun before the answer came proves nothing (questions.go).

// solFollowupFrame writes one audible 20 ms frame through p.
func solFollowupFrame(t *testing.T, p Playback) {
	t.Helper()
	pcm := make([]byte, 960)
	for i := 0; i < len(pcm); i += 2 {
		pcm[i+1] = 16
	}
	if err := p.Write(t.Context(), pcm, audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err != nil {
		t.Fatal(err)
	}
}

// TestSolFollowup195AckTailIsNotAnswer fails if audio that continues the
// acknowledgement's speech after the answer came, with no speech of its own,
// is taken for the answer: a stop then logs the answer as stopped. The
// control plays the answer in a speech begun after it came.
func TestSolFollowup195AckTailIsNotAnswer(t *testing.T) {
	for _, tail := range []bool{false, true} {
		name := "completed answer control"
		if tail {
			name = "the acknowledgement's tail only"
		}
		t.Run(name, func(t *testing.T) {
			r, sess, log := review195Driver(t, newFakeConcierge("ABC-12 is in review."))
			r.heard("what about ABC-12?", true, true)
			ack := sess.x.Out.Begin()
			solFollowupFrame(t, ack)
			r.speaker.FlushSpeech()
			if !tail {
				r.speaker.endSpeech(audio.InputVoice)
				r.speaker.FlushSpeech()
			}
			review195Tool(t, sess, "what about ABC-12?")
			if tail {
				solFollowupFrame(t, ack) // the acknowledgement goes on; the answer is not spoken yet
			} else {
				solFollowupFrame(t, sess.x.Out.Begin())
			}
			r.speaker.endSpeech(audio.InputVoice)
			r.speaker.FlushSpeech()
			r.s.Stop("Wispr, stop")
			want := OutcomeAnswered
			if tail {
				want = OutcomeStopped
			}
			if q := log.next(t); q.Outcome != want {
				t.Fatalf("logged %s, want %s: %+v", q.Outcome, want, q)
			}
			log.none(t, "a second row", 50*time.Millisecond)
		})
	}
}

// TestAnAnswerWithNoSpeechOfItsOwnEndsUnconfirmed fails if an answer a
// driving voice got, followed by no speech of its own before the exchange
// ended with no cut (here its follow-up window closes), is logged as anything
// but unconfirmed, as the README says: nothing failed, and nothing shows it
// was spoken. It was logged failed at 75c3253, a model or voice failure it is
// not (Sol's review there). The control speaks the answer in a speech of its
// own.
func TestAnAnswerWithNoSpeechOfItsOwnEndsUnconfirmed(t *testing.T) {
	for _, ownSpeech := range []bool{false, true} {
		name := "the answer continues the acknowledgement"
		want := OutcomeUnconfirmed
		if ownSpeech {
			name, want = "a speech of its own control", OutcomeAnswered
		}
		t.Run(name, func(t *testing.T) {
			log := &questionLog{ch: make(chan Question, 16)}
			r := newRigWith(t, newFakeConcierge("ABC-12 is in review."), func(cfg *SupervisorConfig, r *rig) {
				cfg.Question = log.add
				v := drivingVoice{r.voice}
				cfg.Voice = func() (Voice, error) { return v, nil }
				cfg.IdleWait = 300 * time.Millisecond
			})
			r.heard("Wispr.", true, true)
			sess := r.waitSession()
			r.heard("what about ABC-12?", true, true)
			ack := sess.x.Out.Begin()
			solFollowupFrame(t, ack)
			r.speaker.FlushSpeech()
			if ownSpeech {
				r.speaker.endSpeech(audio.InputVoice)
				r.speaker.FlushSpeech()
			}
			review195Tool(t, sess, "what about ABC-12?")
			if ownSpeech {
				solFollowupFrame(t, sess.x.Out.Begin())
			} else {
				solFollowupFrame(t, ack)
			}
			r.speaker.FlushSpeech()
			r.speaker.endSpeech(audio.InputVoice)
			r.speaker.FlushSpeech()
			if q := log.next(t); q.Outcome != want {
				t.Fatalf("logged %s, want %s: %+v", q.Outcome, want, q)
			}
		})
	}
}

// solFollowupConcierge answers each turn with a line of its own.
type solFollowupConcierge struct{ *fakeConcierge }

func (c *solFollowupConcierge) Answer(ctx context.Context, x *Exchange, turn string, say SayFunc) error {
	x.Asked = turn
	return say(ctx, "The answer to "+turn, Fact)
}

// TestSolFollowup195EachResultNeedsOwnAudio fails if one speech marks two
// answers played: when two answers have come and one speech plays, only the
// older is answered, and a stop logs the other as stopped. The control plays
// the first answer before the second comes.
func TestSolFollowup195EachResultNeedsOwnAudio(t *testing.T) {
	for _, bothReady := range []bool{false, true} {
		name := "separate answers control"
		if bothReady {
			name = "two answers, one speech"
		}
		t.Run(name, func(t *testing.T) {
			base := newFakeConcierge()
			c := &solFollowupConcierge{base}
			log := &questionLog{ch: make(chan Question, 16)}
			r := newRigWith(t, base, func(cfg *SupervisorConfig, r *rig) {
				cfg.Concierge, cfg.Question = c, log.add
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait = time.Minute
			})
			r.heard("Wispr.", true, true)
			sess := r.waitSession()
			call := func(words string) {
				r.heard(words, true, true)
				args, err := json.Marshal(AnswerArgs{Question: words})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sess.x.Tools.Run(t.Context(), ToolCall{Name: AnswerTool, Args: args}); err != nil {
					t.Fatal(err)
				}
			}
			call("what about ABC-12?")
			if bothReady {
				call("what about ABC-13?")
			}
			solFollowupFrame(t, sess.x.Out.Begin()) // one speech
			r.speaker.FlushSpeech()
			r.speaker.endSpeech(audio.InputVoice)
			r.speaker.FlushSpeech()
			if !bothReady {
				call("what about ABC-13?")
			}
			r.s.Stop("Wispr, stop")
			got := map[string]Question{}
			for range 2 {
				q := log.next(t)
				got[q.Heard] = q
			}
			if q := got["what about ABC-12?"]; q.Outcome != OutcomeAnswered {
				t.Errorf("the answer the speech played: %+v", q)
			}
			if q := got["what about ABC-13?"]; q.Outcome != OutcomeStopped {
				t.Errorf("an answer with no speech of its own: %+v", q)
			}
			log.none(t, "a third row", 50*time.Millisecond)
		})
	}
}
