package voice

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// The follow-up window (R10, as Main set it on 2026-11-19): in the 6 s after
// an answer, an utterance with no wake phrase continues the exchange only
// when Deepgram's transcript punctuates it as a question. Anything else is
// side talk: no voice answers it, and the window closes.

const (
	sideTalk = "Okay, let's move on to the budget review next."
	followUp = "And when will it ship?"
)

// TestSideTalkInTheFollowUpWindowIsNotAnswered fails if side talk said after
// voice A's answer goes to the Concierge, or to Dispatch as a read-ahead, or
// keeps the exchange open.
func TestSideTalkInTheFollowUpWindowIsNotAnswered(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	r.heard(sideTalk, true, true)
	eventually(t, "the window closed on side talk", func() bool { _, _, closed := sess.state(); return closed })
	c.mu.Lock()
	turns := append([]string(nil), c.turns...)
	c.mu.Unlock()
	if slices.Contains(turns, sideTalk) {
		t.Fatalf("side talk went to the Concierge as a turn: %q", turns)
	}
	for _, h := range c.heardCalls() {
		if h.words == sideTalk {
			t.Fatal("side talk was read for in Dispatch while the asker spoke")
		}
	}
}

// TestAQuestionInTheFollowUpWindowContinuesTheExchange fails if a follow-up
// punctuated as a question, with no wake phrase, is not voice A's next turn.
func TestAQuestionInTheFollowUpWindowContinuesTheExchange(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	r.heard(followUp, true, true)
	if got := r.waitTurn(); got != followUp {
		t.Fatalf("follow-up turn %q, want %q", got, followUp)
	}
}

// TestSideTalkToADrivingVoiceGetsNoWords fails if a driving voice's answer
// tool, called on side talk said in the follow-up window, asks the Concierge
// or returns words, or the window stays open; or if a follow-up question
// gets no answer.
func TestSideTalkToADrivingVoiceGetsNoWords(t *testing.T) {
	for _, tc := range []struct {
		said     string
		answered bool
	}{{sideTalk, false}, {followUp, true}} {
		t.Run(tc.said, func(t *testing.T) {
			c := newFakeConcierge("It ships tomorrow.")
			r := newDrivingRigWith(t, time.Minute, c)
			r.s.cfg.FollowUpWait = time.Minute
			r.heard("Wispr, what is happening?", true, true)
			sess := r.waitSession()
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.speaker.finish(audio.InputVoice) // the follow-up window opens
			eventually(t, "the follow-up window opened", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.ex != nil && r.s.ex.followUpTimer != nil
			})
			r.heard(tc.said, true, true)
			out, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatalf("the answer tool: %v", err)
			}
			var res AnswerResult
			if err := json.Unmarshal(out, &res); err != nil {
				t.Fatalf("the answer tool returned %s", out)
			}
			if tc.answered {
				if res.Say != "It ships tomorrow." {
					t.Fatalf("a follow-up question got %+v, want the Concierge's answer", res)
				}
				return
			}
			if res.Say != "" {
				t.Fatalf("side talk got %+v, want no words", res)
			}
			select {
			case got := <-c.asked:
				t.Fatalf("side talk went to the Concierge: %q", got)
			default:
			}
			eventually(t, "the window closed on side talk", func() bool { _, _, closed := sess.state(); return closed })
		})
	}
}

// didYouMean is the Concierge's question when a search has no one clear
// match; the person's answer to it is no question.
const didYouMean = "Did you mean ABC-12, Realtime voice, or ABC-13, Wake word?"

// TestAnAnswerToWisprsQuestionContinuesTheExchange fails if a reply with no
// wake phrase to a question Wispr itself asked ("Did you mean A or B?") is
// taken for side talk because it is no question.
func TestAnAnswerToWisprsQuestionContinuesTheExchange(t *testing.T) {
	t.Run("tts", func(t *testing.T) {
		c := newFakeConcierge()
		c.acks = []string{didYouMean}
		r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
		sess := r.ask("what is happening with the voice work?")
		waitSaid(t, sess)
		r.speaker.finish(audio.InputVoice)
		r.waitEvent("voice_spoken:sess-1")
		r.heard("The first one.", true, true)
		if got := r.waitTurn(); got != "The first one." {
			t.Fatalf("the pick went to the Concierge as %q", got)
		}
	})
	t.Run("driving", func(t *testing.T) {
		c := newFakeConcierge()
		c.acks = []string{didYouMean}
		r := newDrivingRigWith(t, time.Minute, c)
		r.s.cfg.FollowUpWait = time.Minute
		r.heard("Wispr, what is happening with the voice work?", true, true)
		sess := r.waitSession()
		if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("the answer tool: %v", err)
		}
		<-c.asked
		r.waitSpeaking()
		r.speaker.finish(audio.InputVoice)
		eventually(t, "the follow-up window opened", func() bool {
			r.s.mu.Lock()
			defer r.s.mu.Unlock()
			return r.s.ex != nil && r.s.ex.followUpTimer != nil
		})
		r.heard("The first one.", true, true)
		if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d2", Name: AnswerTool, Args: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("the answer tool: %v", err)
		}
		select {
		case got := <-c.asked:
			if got != "The first one." {
				t.Fatalf("the pick went to the Concierge as %q", got)
			}
		default:
			t.Fatal("the pick was taken for side talk")
		}
	})
}
