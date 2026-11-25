package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// TestSideTalkAfterWisprsQuestionClosesTheExchange fails if a reply to
// Wispr's own question that the Concierge takes for side talk (ErrSideTalk:
// it picks nothing offered and asks nothing) gets a word, a sorry or the
// error sound, or leaves the exchange open, for voice A or a driving voice.
func TestSideTalkAfterWisprsQuestionClosesTheExchange(t *testing.T) {
	const reply = "Okay, let's move on to the budget."
	sideTalk := map[string]error{reply: fmt.Errorf("concierge: %w", ErrSideTalk)}
	t.Run("tts", func(t *testing.T) {
		c := newFakeConcierge()
		c.acks = []string{didYouMean}
		c.turnErrs = sideTalk
		r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
		sess := r.ask("what is happening with the voice work?")
		waitSaid(t, sess)
		r.speaker.finish(audio.InputVoice)
		r.waitEvent("voice_spoken:sess-1")
		r.heard(reply, true, true)
		if got := r.waitTurn(); got != reply {
			t.Fatalf("the reply went to the Concierge as %q", got)
		}
		eventually(t, "the exchange closed on side talk", func() bool { _, _, closed := sess.state(); return closed })
		if says := sess.saysCopy(); !slices.Equal(says, []string{didYouMean}) {
			t.Errorf("the voice said %q, want only %q", says, didYouMean)
		}
		if slices.Contains(r.timingEvents(), "error_sound") {
			t.Error("side talk played the error sound")
		}
	})
	t.Run("driving", func(t *testing.T) {
		c := newFakeConcierge()
		c.acks = []string{didYouMean}
		c.turnErrs = sideTalk
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
		eventually(t, "the follow-up window opened after Wispr asked", func() bool {
			r.s.mu.Lock()
			defer r.s.mu.Unlock()
			return r.s.ex != nil && r.s.ex.followUpTimer != nil && r.s.ex.askedBack
		})
		r.heard(reply, true, true)
		out, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d2", Name: AnswerTool, Args: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatalf("the answer tool on side talk: %v", err)
		}
		var res AnswerResult
		if err := json.Unmarshal(out, &res); err != nil || res.Say != "" {
			t.Fatalf("the answer tool on side talk returned %s (%v), want no words", out, err)
		}
		if got := <-c.asked; got != reply {
			t.Fatalf("the reply went to the Concierge as %q", got)
		}
		eventually(t, "the exchange closed on side talk", func() bool { _, _, closed := sess.state(); return closed })
		if slices.Contains(r.timingEvents(), "error_sound") {
			t.Error("side talk played the error sound")
		}
	})
}

// TestAReplyToWisprsQuestionGoesInTheRoomsWords fails if a driving voice's
// answer tool, called on a reply to Wispr's own question, hands the
// Concierge the vendor's own wording rather than the reply Deepgram heard: a
// vendor that rewrote side talk as a question ("What about the budget?")
// would get it past the rule that side talk after Wispr's question is not
// answered (Main, on R5's rewrite). It also fails if a reply with no final
// result within followUpTurnWait reaches the Concierge, gets words, or
// leaves the exchange open: it is side talk.
func TestAReplyToWisprsQuestionGoesInTheRoomsWords(t *testing.T) {
	const said, rewritten = "Okay, let's move on to the budget.", `{"question":"What about the budget?"}`
	askBack := func(t *testing.T, c *fakeConcierge) (*rig, *fakeSession) {
		t.Helper()
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
		eventually(t, "the follow-up window opened after Wispr asked", func() bool {
			r.s.mu.Lock()
			defer r.s.mu.Unlock()
			return r.s.ex != nil && r.s.ex.followUpTimer != nil && r.s.ex.askedBack
		})
		return r, sess
	}
	t.Run("a final reply", func(t *testing.T) {
		c := newFakeConcierge()
		c.turnErrs = map[string]error{said: fmt.Errorf("concierge: %w", ErrSideTalk)}
		r, sess := askBack(t, c)
		r.heard(said, true, true)
		out, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d2", Name: AnswerTool, Args: json.RawMessage(rewritten)})
		if err != nil {
			t.Fatalf("the answer tool: %v", err)
		}
		if got := <-c.asked; got != said {
			t.Fatalf("the Concierge got %q, want the room's words %q", got, said)
		}
		var res AnswerResult
		if err := json.Unmarshal(out, &res); err != nil || res.Say != "" {
			t.Fatalf("the answer tool returned %s (%v), want no words", out, err)
		}
		eventually(t, "the exchange closed on side talk", func() bool { _, _, closed := sess.state(); return closed })
	})
	t.Run("no final reply", func(t *testing.T) {
		c := newFakeConcierge()
		r, sess := askBack(t, c)
		r.heard("Okay, let's move", false, false)
		start := time.Now()
		out, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d2", Name: AnswerTool, Args: json.RawMessage(rewritten)})
		if err != nil {
			t.Fatalf("the answer tool: %v", err)
		}
		var res AnswerResult
		if err := json.Unmarshal(out, &res); err != nil || res.Say != "" {
			t.Fatalf("the answer tool returned %s (%v), want no words", out, err)
		}
		select {
		case got := <-c.asked:
			t.Fatalf("a reply with no final went to the Concierge as %q", got)
		default:
		}
		if took := time.Since(start); took < followUpTurnWait {
			t.Errorf("the answer tool gave up after %v, want it to wait %v for the final", took, followUpTurnWait)
		}
		eventually(t, "the exchange closed on side talk", func() bool { _, _, closed := sess.state(); return closed })
	})
}

// TestATurnsHeardAndAnswerCarryItsNumber fails if the Heard calls of a turn
// and the Answer of that turn carry different numbers (Exchange.Turn), or a
// later turn's are not greater: the Concierge reads nothing ahead for a turn
// once it began to answer it, the Heard of which the Supervisor can deliver
// after the answer started, and still reads ahead for the next turn (Sol's
// review of #191 at 1cbcb494).
func TestATurnsHeardAndAnswerCarryItsNumber(t *testing.T) {
	const reply = "The first one."
	check := func(t *testing.T, c *fakeConcierge) {
		t.Helper()
		eventually(t, "the reply's Heard", func() bool {
			return slices.ContainsFunc(c.heardCalls(), func(h heardCall) bool { return h.words == reply })
		})
		c.mu.Lock()
		exchanges := slices.Clone(c.exchanges)
		c.mu.Unlock()
		if len(exchanges) != 2 {
			t.Fatalf("%d answers, want the question's and the reply's", len(exchanges))
		}
		if exchanges[1].Turn <= exchanges[0].Turn {
			t.Errorf("the reply's answer carries turn %d, the question's %d: want a greater one", exchanges[1].Turn, exchanges[0].Turn)
		}
		for _, h := range c.heardCalls() {
			for _, x := range exchanges {
				if h.words == x.AskerTurn && h.x.Turn != x.Turn {
					t.Errorf("Heard(%q) carries turn %d, its answer %d", h.words, h.x.Turn, x.Turn)
				}
			}
		}
	}
	t.Run("tts", func(t *testing.T) {
		c := newFakeConcierge()
		c.acks = []string{didYouMean}
		r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
		sess := r.ask("what is happening with the voice work?")
		waitSaid(t, sess)
		r.speaker.finish(audio.InputVoice)
		r.waitEvent("voice_spoken:sess-1")
		r.heard(reply, true, true)
		r.waitTurn()
		check(t, c)
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
		eventually(t, "the follow-up window opened after Wispr asked", func() bool {
			r.s.mu.Lock()
			defer r.s.mu.Unlock()
			return r.s.ex != nil && r.s.ex.followUpTimer != nil && r.s.ex.askedBack
		})
		r.heard(reply, true, true)
		if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d2", Name: AnswerTool, Args: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("the answer tool: %v", err)
		}
		<-c.asked
		check(t, c)
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

// TestAKeyRightAfterTheNameReachesTheConciergeWritten fails if a key said
// right after the wake phrase in the form a date or a thousand takes ("Wispr,
// in for 10/27?") is not written as the key in the turn: those words follow
// the name, where "in" cannot begin a phrase (Sol's review of #135 at
// 21bbef7). The control is a reply to Wispr's own question with no name,
// "In for five thousand dollars?", which stays an amount, as on main.
func TestAKeyRightAfterTheNameReachesTheConciergeWritten(t *testing.T) {
	c := newFakeConcierge()
	c.acks = []string{didYouMean}
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) {
		cfg.ProjectKeys = func() []string { return []string{"INFRA"} }
		cfg.FollowUpWait = time.Minute
	})
	sess := r.ask("in for 10/27?")
	c.mu.Lock()
	turns := append([]string(nil), c.turns...)
	c.mu.Unlock()
	if len(turns) == 0 || turns[0] != "INFRA-1027?" {
		t.Fatalf("the Concierge got %q, want %q", turns, "INFRA-1027?")
	}
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	c.acks = nil
	r.heard("In for five thousand dollars?", true, true)
	if got, want := r.waitTurn(), "In for five thousand dollars?"; got != want {
		t.Fatalf("the reply with no name reached the Concierge as %q, want %q", got, want)
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
// only after the call's own reply (the model speaks in the same reply as its
// call), so waiting holds the answer until the Say gives up. A routing line
// that never played is dropped when the answer is a fact ready within 3 s of
// t0, and else leads the answer as one spoken sentence (Main, on GWDgAgent's
// gap H: the room heard 9 s of nothing).
func TestARoutingLineDoesNotHoldTheAnswerTool(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ack   string
		mode  SayMode
		delay time.Duration // before the Concierge's fact
		// returns: how long after the fact the Concierge's Answer returns,
		// as when it waits on an agent's send: the bound reads the fact.
		returns time.Duration
		// said: how long before its final came the asker said the turn's
		// last word: the bound counts from when it was said.
		said time.Duration
		want string
	}{
		{"a fact ready within the bound", "Reading ABC-12.", Ack, 0, 0, 0, "The issue is in review."},
		{"a fact later than the bound", "Reading ABC-12.", Ack, 1500 * time.Millisecond, 0, 0, "Reading ABC-12. The issue is in review."},
		{"a fact within the bound, the answer returning later", "Reading ABC-12.", Ack, 0, 1500 * time.Millisecond, 0, "The issue is in review."},
		{"a fact soon after a final that came late", "Reading ABC-12.", Ack, 0, 0, 2 * time.Second, "Reading ABC-12. The issue is in review."},
		// R5's line naming the issue a search chose always leads (Main).
		{"R5's line, a fact within the bound", "I took ABC-12.", Lead, 0, 0, 0, "I took ABC-12. The issue is in review."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bound := leadLineDropWithin
			leadLineDropWithin = time.Second // above leadLineWait, the routing line's 0.5 s to start
			t.Cleanup(func() { leadLineDropWithin = bound })
			c := newFakeConcierge("The issue is in review.")
			c.acks, c.ackMode = []string{tc.ack}, tc.mode
			c.afterAcks = make(chan struct{})
			if tc.returns > 0 {
				c.afterLines = make(chan struct{})
				time.AfterFunc(tc.delay+tc.returns, func() { close(c.afterLines) })
			}
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				cfg.Voice = func() (Voice, error) { return heldAckVoice{r: r, release: release}, nil }
				cfg.IdleWait = time.Minute
			})
			const question = "Wispr, what is happening with ABC-12?"
			if tc.said > 0 {
				words := len(strings.Fields(question))
				r.s.OnTranscript(timedResult(question, 0, true, from(time.Now().Add(-tc.said-time.Duration(words)*200*time.Millisecond))))
			} else {
				r.heard(question, true, true)
			}
			sess := r.waitSession()
			out := make(chan json.RawMessage, 1)
			go func() {
				res, _ := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)})
				out <- res
			}()
			time.AfterFunc(tc.delay, func() { close(c.afterAcks) })
			select {
			case res := <-out:
				var got AnswerResult
				if err := json.Unmarshal(res, &got); err != nil || got.Say != tc.want {
					t.Fatalf("the answer tool returned %s, want %q", res, tc.want)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("the answer tool waited on the routing line's Say")
			}
			never(t, "the held lead line spoken as well", 200*time.Millisecond, func() bool { return slices.Contains(sess.saysCopy(), tc.ack) })
		})
	}
}

// TestTheLeadLineIsSpokenAtOnce fails if R5's line naming the issue a search
// chose waits for the answer tool's result instead of being spoken at once,
// as the routing line is, or is spoken twice once it played to its end.
func TestTheLeadLineIsSpokenAtOnce(t *testing.T) {
	c := newFakeConcierge("The issue is in review.")
	c.acks, c.ackMode = []string{"I took ABC-12."}, Lead
	c.afterAcks = make(chan struct{})
	r := newDrivingRigWith(t, time.Minute, c)
	r.heard("Wispr, what is happening with the voice work?", true, true)
	sess := r.waitSession()
	result := make(chan json.RawMessage, 1)
	go func() {
		out, _ := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)})
		result <- out
	}()
	if got := waitSaid(t, sess); got != "I took ABC-12." {
		t.Fatalf("said %q at once, want R5's line", got)
	}
	r.speaker.finish(audio.InputVoice) // it plays to its end, with no cut
	close(c.afterAcks)
	var res AnswerResult
	if err := json.Unmarshal(<-result, &res); err != nil || res.Say != "The issue is in review." {
		t.Fatalf("the answer tool returned %+v (%v), want the fact alone", res, err)
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
	if mode == Ack || mode == Lead {
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Session.Say(ctx, text, mode)
}

// TestARoutingLineThatBeganDoesNotLeadTheAnswer fails if a routing line
// whose speech began and was then cut leads the answer, as a line that never
// played does: the room heard its start, and would hear it again. In session
// 20261121011434 the barge-in cut "Reading INFRA-1027." at "readi-", and the
// answer began with the line once more. It also fails if R5's line naming the
// issue a search chose, cut by a barge-in or by a stop at the speaker, does
// not lead the answer, whether its Say then failed or succeeded, and whenever
// the fact was ready: the Supervisor's own cut can end it before the issue's
// name.
func TestARoutingLineThatBeganDoesNotLeadTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		mode SayMode
		// factFirst: the fact is ready while the line plays, before the cut;
		// else after leadLineDropWithin, when a routing line that never
		// played would lead it.
		factFirst bool
		// okAfterCut: the cut Say still returns with no error, as GPT-Live's
		// does; early: it returns with no error at once, while the line still
		// plays, as the Voice Agent's does.
		okAfterCut, early bool
		// stop: the Supervisor cuts the line with no transcript, as a stop
		// does (cutVoice), not a barge-in.
		stop bool
		want string
	}{
		{"a routing line", "Reading ABC-12.", Ack, false, false, false, false, "The issue is in review."},
		{"a routing line, stopped", "Reading ABC-12.", Ack, false, false, false, true, "The issue is in review."},
		{"R5's line naming the issue", "I took ABC-12.", Lead, false, false, false, false, "I took ABC-12. The issue is in review."},
		{"R5's line, stopped", "I took ABC-12.", Lead, false, false, false, true, "I took ABC-12. The issue is in review."},
		{"R5's line, cut after the fact was ready", "I took ABC-12.", Lead, true, false, false, false, "I took ABC-12. The issue is in review."},
		{"R5's line, cut, its Say succeeding", "I took ABC-12.", Lead, false, true, false, false, "I took ABC-12. The issue is in review."},
		{"R5's line, stopped, its Say succeeding", "I took ABC-12.", Lead, false, true, false, true, "I took ABC-12. The issue is in review."},
		{"R5's line, cut after the fact was ready, its Say succeeding", "I took ABC-12.", Lead, true, true, false, false, "I took ABC-12. The issue is in review."},
		{"R5's line, its Say done while it plays, cut after the fact was ready", "I took ABC-12.", Lead, true, false, true, false, "I took ABC-12. The issue is in review."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bound := leadLineDropWithin
			leadLineDropWithin = time.Second // above leadLineWait, the routing line's 0.5 s to start
			t.Cleanup(func() { leadLineDropWithin = bound })
			c := newFakeConcierge("The issue is in review.")
			c.acks, c.ackMode = []string{tc.line}, tc.mode
			c.afterAcks = make(chan struct{})
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				cfg.Voice = func() (Voice, error) { return cutLineVoice{r: r, okAfterCut: tc.okAfterCut, early: tc.early}, nil }
				cfg.IdleWait = time.Minute
				cfg.FollowUpWait = time.Minute
			})
			r.heard("Wispr, what is happening with ABC-12?", true, true)
			sess := r.waitSession()
			out := make(chan json.RawMessage, 1)
			go func() {
				res, _ := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)})
				out <- res
			}()
			started := waitSpeechAfter(t, r, time.Time{}) // the line begins
			if tc.factFirst {
				close(c.afterAcks)
				time.Sleep(200 * time.Millisecond) // the answer tool has the fact
			}
			if tc.stop {
				cutVoice(t, r)
			} else {
				r.s.OnTranscript(timedResult("wait, who holds it?", 200, false, from(started.Add(300*time.Millisecond))))
			}
			eventually(t, "the line to be cut", func() bool { return voiceCutAfter(r, started) })
			if !tc.factFirst {
				time.AfterFunc(1500*time.Millisecond, func() { close(c.afterAcks) })
			}
			select {
			case res := <-out:
				var got AnswerResult
				if err := json.Unmarshal(res, &got); err != nil || got.Say != tc.want {
					t.Fatalf("the answer tool returned %s, want %q", res, tc.want)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("the answer tool did not return")
			}
		})
	}
}

// TestALeadThatPlaysWholeIsNotSaidAgain fails if the answer tool, with its
// fact ready while R5's line naming the issue still plays, does not wait for
// the line to end, or then leads the answer with it again: a line whose Say
// returned as it began, as the Voice Agent's could, played whole (Sol's review
// of #133 at 2f101fa).
func TestALeadThatPlaysWholeIsNotSaidAgain(t *testing.T) {
	c := newFakeConcierge("The issue is in review.")
	c.acks, c.ackMode = []string{"I took ABC-12."}, Lead
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		cfg.Voice = func() (Voice, error) { return cutLineVoice{r: r, early: true}, nil }
		cfg.IdleWait = time.Minute
		cfg.FollowUpWait = time.Minute
	})
	r.heard("Wispr, what is happening with ABC-12?", true, true)
	sess := r.waitSession()
	out := make(chan json.RawMessage, 1)
	go func() {
		res, _ := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)})
		out <- res
	}()
	waitSpeechAfter(t, r, time.Time{}) // the line plays; its Say has returned, and the fact is ready
	select {
	case res := <-out:
		t.Fatalf("the answer tool returned %s while the line played", res)
	case <-time.After(300 * time.Millisecond):
	}
	r.speaker.finish(audio.InputVoice) // the line plays to its end
	select {
	case res := <-out:
		var got AnswerResult
		if err := json.Unmarshal(res, &got); err != nil || got.Say != "The issue is in review." {
			t.Fatalf("the answer tool returned %s, want the fact alone: the line played whole", res)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the answer tool did not return")
	}
}

// TestALeadThatContinuesAnEarlierSpeechIsNotSaidAgain fails if R5's line
// naming the issue, played whole with no cut as more of a speech already
// under way, leads the answer again: no new speech began after it was
// called, yet the room heard it (Sol's review of #133 at 795ad04). It also
// fails if the answer tool returns while that speech, and the line with it,
// still plays, or if a cut of it then does not make the line lead the answer
// (Sol's review of #133 at f398ca8 found the line accepted at once).
func TestALeadThatContinuesAnEarlierSpeechIsNotSaidAgain(t *testing.T) {
	for _, cut := range []bool{false, true} {
		t.Run(fmt.Sprintf("cut %v", cut), func(t *testing.T) {
			c := newFakeConcierge("The issue is in review.")
			c.acks, c.ackMode = []string{"I took ABC-12."}, Lead
			c.afterAcks = make(chan struct{})
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait = time.Minute
				cfg.FollowUpWait = time.Minute
			})
			r.heard("Wispr, what is happening with ABC-12?", true, true)
			sess := r.waitSession()
			speakAs(t, sess, "Let me look.") // an earlier speech, under way
			waitSpeechAfter(t, r, time.Time{})
			out := make(chan json.RawMessage, 1)
			go func() {
				res, _ := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)})
				out <- res
			}()
			waitSaid(t, sess) // the line goes to the voice, and plays as more of that speech
			close(c.afterAcks)
			select {
			case res := <-out:
				t.Fatalf("the answer tool returned %s while the line still played", res)
			case <-time.After(300 * time.Millisecond):
			}
			want := "The issue is in review."
			if cut {
				cutVoice(t, r)
				want = "I took ABC-12. " + want
			} else {
				r.speaker.finish(audio.InputVoice) // the speech, the line with it, plays to its end
			}
			select {
			case res := <-out:
				var got AnswerResult
				if err := json.Unmarshal(res, &got); err != nil || got.Say != want {
					t.Fatalf("the answer tool returned %s, want %q", res, want)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("the answer tool did not return")
			}
		})
	}
}

// TestALeadStoppedBeforeItsEventsAreDeliveredLeadsTheAnswer fails if R5's
// line naming the issue, cut after its Say succeeded, does not lead the
// answer when the speaker has not yet delivered the speech's start and its
// end (Sol's review of #133 at b6eef5c). It also fails if, uncut, the answer
// tool returns while the line plays, its start not yet delivered: the tool
// must take in the speaker's queued events to see the voice speaking (Sol's
// review of #133 at f398ca8 found the check blind to that). The control,
// uncut and played to its end, does not lead the answer.
func TestALeadStoppedBeforeItsEventsAreDeliveredLeadsTheAnswer(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		t.Run(fmt.Sprintf("stopped %v", stopped), func(t *testing.T) {
			c := newFakeConcierge("The issue is in review.")
			c.acks, c.ackMode = []string{"I took ABC-12."}, Lead
			c.afterAcks = make(chan struct{})
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait = time.Minute
				cfg.FollowUpWait = time.Minute
			})
			r.heard("Wispr, what is happening with ABC-12?", true, true)
			sess := r.waitSession()
			release := r.speaker.holdEvents()
			t.Cleanup(release)
			out := make(chan json.RawMessage, 1)
			go func() {
				res, _ := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)})
				out <- res
			}()
			waitSaid(t, sess) // the line's Say succeeded; its start waits for delivery
			want := "The issue is in review."
			if stopped {
				cutVoice(t, r) // its cancelled end waits for delivery too
				want = "I took ABC-12. " + want
				close(c.afterAcks)
			} else {
				close(c.afterAcks)
				select {
				case res := <-out:
					t.Fatalf("the answer tool returned %s while the line played, its start not yet delivered", res)
				case <-time.After(300 * time.Millisecond):
				}
				r.speaker.finish(audio.InputVoice)
			}
			select {
			case res := <-out:
				var got AnswerResult
				if err := json.Unmarshal(res, &got); err != nil || got.Say != want {
					t.Fatalf("the answer tool returned %s, want %q", res, want)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("the answer tool did not return")
			}
		})
	}
}

// cutLineVoice opens the rig's fake sessions, as a voice that drives its own
// turn, whose Ack and Lead Says return, as the real voices' do, only once
// their speech has played: here, once the voice input is cut, with the cut's
// error, or with none when okAfterCut is set; or, with early, at once.
type cutLineVoice struct {
	r                 *rig
	okAfterCut, early bool
}

func (cutLineVoice) DrivesTurn() bool { return true }

func (v cutLineVoice) Open(ctx context.Context, x *Exchange) (Session, error) {
	s, err := v.r.voice.Open(ctx, x)
	if err != nil {
		return nil, err
	}
	return cutLineSession{Session: s, r: v.r, okAfterCut: v.okAfterCut, early: v.early}, nil
}

type cutLineSession struct {
	Session
	r                 *rig
	okAfterCut, early bool
}

func (p cutLineSession) Say(ctx context.Context, text string, mode SayMode) error {
	began := time.Now()
	if err := p.Session.Say(ctx, text, mode); err != nil || (mode != Ack && mode != Lead) || p.early {
		return err
	}
	for !voiceCutAfter(p.r, began) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	if p.okAfterCut {
		return nil
	}
	return errFakeCancelled
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
	for i := range xs {
		marks[i] = xs[i].FollowUpTurn
	}
	return marks
}
