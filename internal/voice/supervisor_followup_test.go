package voice

import (
	"context"
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

// TestASpokenKeyReachesTheConciergeWritten fails if a key of one of
// Dispatch's projects, said as Deepgram writes it ("in for forty five ninety
// two", end-to-end run 8c), reaches the Concierge as words it cannot read as
// a key (R4).
func TestASpokenKeyReachesTheConciergeWritten(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) {
		cfg.ProjectKeys = func() []string { return []string{"INFRA", "ABC"} }
	})
	r.ask("please ask the agent on in for forty five ninety two when slice one ships?")
	c.mu.Lock()
	turns := append([]string(nil), c.turns...)
	c.mu.Unlock()
	if want := "please ask the agent on INFRA-4592 when slice one ships?"; len(turns) == 0 || turns[0] != want {
		t.Fatalf("the Concierge got %q, want %q", turns, want)
	}
}

// TestASpokenKeySplitAcrossResultsReachesTheConciergeWritten fails if a key
// whose words came in two of Deepgram's results ("…agent on in" then "For
// 4,592 when…", end-to-end run 8e) is not written as the key in the turn.
func TestASpokenKeySplitAcrossResultsReachesTheConciergeWritten(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) {
		cfg.ProjectKeys = func() []string { return []string{"INFRA"} }
	})
	r.heard("Wispr, please ask the agent on in", true, false)
	r.waitSession()
	r.heard("For 4,592 when slice one will be ready to try?", true, true)
	if got, want := r.waitTurn(), "please ask the agent on INFRA-4592 when slice one will be ready to try?"; got != want {
		t.Fatalf("the Concierge got %q, want %q", got, want)
	}
}

// TestAReplyToWisprsQuestionKeepsTheFloorFree fails if a person's reply to
// Wispr's own question ("The first one.") counts as room speech against R7's
// free floor: it is part of the exchange, whose last turn is R7's t0, so the
// holder's answer must speak by itself (end-to-end run 8d needed "Wispr, go
// ahead").
func TestAReplyToWisprsQuestionKeepsTheFloorFree(t *testing.T) {
	c := newFakeConcierge()
	c.acks = []string{didYouMean}
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
	sess := r.ask("please ask the agent who holds the voice work when it ships?")
	asked := time.Now() // the Concierge's task: its question, in this exchange
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	c.acks = nil
	r.heard("The first one.", true, true)
	r.waitTurn()
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", IssueKey: "ABC-12", Text: "It ships tomorrow.", AskedAt: asked}
	if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
		t.Fatalf("said %q, want the holder's answer by itself", got)
	}
}

// TestAnAnswerInTheFollowUpWindowNeedNotWaitForItsEnd fails if a holder's
// answer that lands while an exchange only waits in its follow-up window, with
// nobody speaking, waits for that window to run out before its sound plays
// and it speaks (run 8d: the answer sound came 13.5 s after the answer).
func TestAnAnswerInTheFollowUpWindowNeedNotWaitForItsEnd(t *testing.T) {
	c := newFakeConcierge("I asked the agent on ABC-12.")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
	sess := r.ask("please ask the agent on ABC-12 when it ships?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1") // the window opens, for a minute
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", IssueKey: "ABC-12", Text: "It ships tomorrow.", AskedAt: time.Now()}
	eventually(t, "the answer sound", func() bool { return slices.Contains(r.speaker.soundsPlayed(), "answer") })
	if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
		t.Fatalf("said %q, want the holder's answer", got)
	}
}

// TestAFollowUpGetsItsWholeTurn fails if the Concierge gets, as the
// exchange's AskerTurn, the interim prefix of a follow-up whose final came
// while the answer tool waited for it (GWDgAgent's follow_up_after_answer-2:
// "And who").
func TestAFollowUpGetsItsWholeTurn(t *testing.T) {
	c := newFakeConcierge("The holder is the agent on the issue.")
	r := newDrivingRigWith(t, time.Minute, c)
	r.s.cfg.FollowUpWait = time.Minute
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	speakAs(t, sess, "The issue is in review since this morning.")
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the follow-up window opened", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex != nil && r.s.ex.followUpTimer != nil
	})
	r.heard("And who", false, false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"And who holds that issue?"}`)})
	}()
	time.Sleep(100 * time.Millisecond)
	r.heard("And who holds that issue?", true, true)
	<-done
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.exchanges) == 0 || c.exchanges[len(c.exchanges)-1].AskerTurn != "And who holds that issue?" {
		t.Fatalf("the Concierge's exchanges %+v, want the whole follow-up as AskerTurn", c.exchanges)
	}
}

// TestARoutingLineDoesNotHoldTheAnswerTool fails if the answer tool waits for
// the routing line's Say: a vendor such as Deepgram's Voice Agent plays it
// only after the call's own reply, so waiting holds the answer until the
// Say gives up.
func TestARoutingLineDoesNotHoldTheAnswerTool(t *testing.T) {
	c := newFakeConcierge("The issue is in review.")
	c.acks = []string{"Reading ABC-12."}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		cfg.Voice = func() (Voice, error) { return heldAckVoice{r: r, release: release}, nil }
		cfg.IdleWait = time.Minute
	})
	r.heard("Wispr, what is happening with ABC-12?", true, true)
	sess := r.waitSession()
	out := make(chan json.RawMessage, 1)
	go func() {
		res, _ := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)})
		out <- res
	}()
	select {
	case res := <-out:
		var got AnswerResult
		if err := json.Unmarshal(res, &got); err != nil || got.Say != "The issue is in review." {
			t.Fatalf("the answer tool returned %s", res)
		}
	case <-time.After(time.Second):
		t.Fatal("the answer tool waited on the routing line's Say")
	}
}

// heldAckVoice opens the rig's fake sessions, as a voice that drives its own
// turn, whose Ack Says do not return until release.
type heldAckVoice struct {
	r       *rig
	release chan struct{}
}

func (heldAckVoice) DrivesTurn() bool { return true }

func (v heldAckVoice) Open(ctx context.Context, x *Exchange) (Session, error) {
	s, err := v.r.voice.Open(ctx, x)
	if err != nil {
		return nil, err
	}
	return heldAckSession{Session: s, release: v.release}, nil
}

type heldAckSession struct {
	Session
	release chan struct{}
}

func (p heldAckSession) Say(ctx context.Context, text string, mode SayMode) error {
	if mode == Ack {
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Session.Say(ctx, text, mode)
}

// TestAFollowUpTurnIsMarked fails if the Concierge cannot tell a turn said in
// the follow-up window with no wake phrase from a call, which it must not
// ask an agent about unless the turn says "ask" (ReviewPR89's round 2).
func TestAFollowUpTurnIsMarked(t *testing.T) {
	t.Run("tts", func(t *testing.T) {
		c := newFakeConcierge("The voice issue is in review.")
		r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
		sess := r.ask("what is happening?")
		waitSaid(t, sess)
		r.speaker.finish(audio.InputVoice)
		r.waitEvent("voice_spoken:sess-1")
		r.heard(followUp, true, true)
		r.waitTurn()
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.exchanges) != 2 || c.exchanges[0].FollowUpTurn || !c.exchanges[1].FollowUpTurn {
			t.Fatalf("the Concierge's exchanges marked %v, want the call unmarked and the follow-up marked", followUpMarks(c.exchanges))
		}
	})
	t.Run("driving", func(t *testing.T) {
		c := newFakeConcierge("It ships tomorrow.")
		r := newDrivingRigWith(t, time.Minute, c)
		r.s.cfg.FollowUpWait = time.Minute
		r.heard("Wispr, what is happening?", true, true)
		sess := r.waitSession()
		speakAs(t, sess, "The issue is in review since this morning.")
		r.waitSpeaking()
		r.speaker.finish(audio.InputVoice)
		eventually(t, "the follow-up window opened", func() bool {
			r.s.mu.Lock()
			defer r.s.mu.Unlock()
			return r.s.ex != nil && r.s.ex.followUpTimer != nil
		})
		r.heard(followUp, true, true)
		if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("the answer tool: %v", err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.exchanges) == 0 || !c.exchanges[len(c.exchanges)-1].FollowUpTurn {
			t.Fatalf("the Concierge's exchanges marked %v, want the follow-up marked", followUpMarks(c.exchanges))
		}
	})
}

func followUpMarks(xs []Exchange) []bool {
	marks := make([]bool, len(xs))
	for i, x := range xs {
		marks[i] = x.FollowUpTurn
	}
	return marks
}
