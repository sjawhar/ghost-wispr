package voice

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// drivingVoice is a voice whose vendor drives the turn, as voices B and C do.
type drivingVoice struct{ *fakeVoice }

func (drivingVoice) DrivesTurn() bool { return true }

func newDrivingRig(t *testing.T, idle time.Duration) *rig {
	t.Helper()
	return newDrivingRigWith(t, idle, newFakeConcierge())
}

// newDrivingRigWith is newDrivingRig over the Concierge c.
func newDrivingRigWith(t *testing.T, idle time.Duration, c *fakeConcierge) *rig {
	t.Helper()
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		v := drivingVoice{r.voice}
		cfg.Voice = func() (Voice, error) { return v, nil }
		cfg.IdleWait = idle
	})
	return r
}

// speakAs makes the vendor speak in sess, as its own speech arrives.
func speakAs(t *testing.T, sess *fakeSession, text string) {
	t.Helper()
	if err := sess.Say(t.Context(), text, Answer); err != nil {
		t.Fatalf("the vendor's speech: %v", err)
	}
	waitSaid(t, sess)
}

// TestADriverVoiceThatFailsEndsTheExchange fails if a voice that drives its
// own turn reports a failure (its connection dropped) and the exchange stays
// open, holding the floor.
func TestADriverVoiceThatFailsEndsTheExchange(t *testing.T) {
	r := newDrivingRig(t, time.Minute)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	sess.fail(errors.New("the vendor closed the connection"))
	eventually(t, "the session closed after its failure", func() bool { _, _, closed := sess.state(); return closed })
	if err := r.s.WithFloor(t.Context(), func() {}); err != nil {
		t.Fatal(err)
	}
}

// TestAVoiceThatEndsByItselfEndsTheExchange fails if a session whose events
// end without a Close leaves its exchange holding the floor.
func TestAVoiceThatEndsByItselfEndsTheExchange(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r := newRig(t, c)
	sess := r.ask("what is happening?")
	sess.die()
	eventually(t, "the exchange ended", func() bool {
		select {
		case got := <-c.cancelled:
			return !got.IsZero()
		default:
			return false
		}
	})
	done := make(chan error, 1)
	go func() { done <- r.s.WithFloor(t.Context(), func() {}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testWait):
		t.Fatal("the floor is still held by a voice that ended")
	}
}

// TestADriverBargeInStopsTheSpeechAndKeepsTheSession fails if a barge-in over
// a voice that drives its own turn does not stop its speech at once, or ends
// its session, which must stay open to hear and answer the interruption.
func TestADriverBargeInStopsTheSpeechAndKeepsTheSession(t *testing.T) {
	r := newDrivingRig(t, time.Minute)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	speakAs(t, sess, "The issue is in review since this morning.")
	r.waitSpeaking()
	r.heard("wait, who holds it?", false, false)
	eventually(t, "the speech interrupted", func() bool { i, _, _ := sess.state(); return !i.IsZero() })
	if _, ok := r.speaker.cancelledAt(audio.InputVoice); !ok {
		t.Error("barge-in did not cancel the voice input")
	}
	never(t, "barge-in closed a driving voice's session", 300*time.Millisecond, func() bool { _, _, closed := sess.state(); return closed })
}

// TestAnIdleDriverExchangeEnds fails if an exchange with a voice that drives
// its own turn stays open when nobody and nothing has spoken for IdleWait, or
// ends while its voice still speaks.
func TestAnIdleDriverExchangeEnds(t *testing.T) {
	r := newDrivingRig(t, 300*time.Millisecond)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	speakAs(t, sess, "The issue is in review since this morning.")
	r.waitSpeaking()
	never(t, "an exchange ended while its voice spoke", 600*time.Millisecond, func() bool { _, _, closed := sess.state(); return closed })
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the idle exchange ended", func() bool { _, _, closed := sess.state(); return closed })
	r.waitEvent("voice_spoken:sess-1")
}

// TestADriverExchangeEndsAFollowUpWaitAfterItsSpeech fails if an exchange
// with a voice that drives its own turn stays open longer than FollowUpWait
// after the voice's last word when nobody speaks (R10): the room's side talk
// after it must not reach the vendor as a turn.
func TestADriverExchangeEndsAFollowUpWaitAfterItsSpeech(t *testing.T) {
	r := newDrivingRig(t, time.Minute)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	speakAs(t, sess, "The issue is in review since this morning.")
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the exchange ended FollowUpWait after the speech", func() bool { _, _, closed := sess.state(); return closed })
}

// TestADriverFollowUpKeepsTheExchangeForATurn fails if a person who speaks in
// the follow-up window does not keep a driving voice's exchange open for the
// vendor's answer.
func TestADriverFollowUpKeepsTheExchangeForATurn(t *testing.T) {
	r := newDrivingRig(t, time.Minute)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	speakAs(t, sess, "The issue is in review since this morning.")
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	r.heard("And who holds it?", true, true)
	never(t, "the exchange ended during the person's turn", 500*time.Millisecond, func() bool { _, _, closed := sess.state(); return closed })
}

// TestAWakeOverADrivingVoiceBargesIn fails if "Wispr, …" said while a voice
// that drives its own turn speaks does not stop its speech and interrupt its
// vendor, or ends its session, or does not make the new words the turn.
func TestAWakeOverADrivingVoiceBargesIn(t *testing.T) {
	c := newFakeConcierge("The holder is the agent on the issue.")
	r := newDrivingRigWith(t, time.Minute, c)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	speakAs(t, sess, "The issue is in review since this morning.")
	r.waitSpeaking()
	r.heard("Wispr, who holds that issue?", true, true)
	eventually(t, "the vendor interrupted", func() bool { i, _, _ := sess.state(); return !i.IsZero() })
	if _, ok := r.speaker.cancelledAt(audio.InputVoice); !ok {
		t.Error("the wake did not cancel the voice input")
	}
	if _, _, closed := sess.state(); closed {
		t.Fatal("the wake closed the driving voice's session")
	}
	if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("the answer tool: %v", err)
	}
	if got := <-c.asked; got != "who holds that issue?" {
		t.Fatalf("the Concierge was asked %q, want the new words", got)
	}
}

// TestADriverAskerTurnFollowsTheLatestTurn fails if the answer tool, called
// with no question, gets a driving voice's first turn after a later one.
func TestADriverAskerTurnFollowsTheLatestTurn(t *testing.T) {
	c := newFakeConcierge("The holder is the agent on the issue.")
	r := newDrivingRigWith(t, time.Minute, c)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	speakAs(t, sess, "The issue is in review since this morning.")
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	r.heard("And who holds it?", true, true)
	if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("the answer tool: %v", err)
	}
	if got := <-c.asked; got != "And who holds it?" {
		t.Fatalf("the Concierge was asked %q, want the latest turn", got)
	}
}

// TestAToolCallKeepsTheDriverExchangeOpen fails if a driving voice's exchange
// ends for idleness while the Concierge still works on its answer tool.
func TestAToolCallKeepsTheDriverExchangeOpen(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r := newDrivingRigWith(t, 200*time.Millisecond, c)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	go func() {
		_, _ = sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)})
	}()
	<-c.asked
	never(t, "the exchange ended while the Concierge answered", 700*time.Millisecond, func() bool { _, _, closed := sess.state(); return closed })
}

// TestTheAnswerToolSpeaksTheRoutingLineAtOnce fails if the Concierge's Ack
// line said before any fact waits for the tool's result instead of being
// spoken at once, or if a fact is spoken rather than returned.
func TestTheAnswerToolSpeaksTheRoutingLineAtOnce(t *testing.T) {
	c := newFakeConcierge("The issue is in review.")
	c.acks = []string{"Reading ABC-12."}
	c.afterAcks = make(chan struct{})
	r := newDrivingRigWith(t, time.Minute, c)
	r.heard("Wispr, what is happening with ABC-12?", true, true)
	sess := r.waitSession()
	result := make(chan json.RawMessage, 1)
	go func() {
		out, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"what is happening with ABC-12?"}`)})
		if err != nil {
			t.Errorf("the answer tool: %v", err)
		}
		result <- out
	}()
	if got := waitSaid(t, sess); got != "Reading ABC-12." {
		t.Fatalf("said %q at once, want the routing line", got)
	}
	close(c.afterAcks)
	var res AnswerResult
	if err := json.Unmarshal(<-result, &res); err != nil || res.Say != "The issue is in review." {
		t.Fatalf("the answer tool returned %+v (%v), want the fact alone", res, err)
	}
	if says := sess.saysCopy(); len(says) != 1 {
		t.Fatalf("the session spoke %q, want the routing line alone", says)
	}
}

// TestAFailedAnswerToolSpeaksTheConciergesLine fails if a failed answer tool
// drops the Concierge's own line for the failure, or plays no error sound.
func TestAFailedAnswerToolSpeaksTheConciergesLine(t *testing.T) {
	c := newFakeConcierge()
	c.acks = []string{"I cannot reach Dispatch yet."}
	c.answerErr = ErrUnreachable
	r := newDrivingRigWith(t, time.Minute, c)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("the answer tool returned %v, want ErrUnreachable", err)
	}
	if says := sess.saysCopy(); !slices.Equal(says, []string{"I cannot reach Dispatch yet."}) {
		t.Fatalf("the session spoke %q, want the Concierge's line", says)
	}
	eventually(t, "the error sound", func() bool { return slices.Contains(r.speaker.soundsPlayed(), "error") })
}

// TestAFailedAnswerToolWithNoLineSaysSorry fails if a failed answer tool for
// which the Concierge said nothing leaves the room with no word.
func TestAFailedAnswerToolWithNoLineSaysSorry(t *testing.T) {
	c := newFakeConcierge()
	c.answerErr = errors.New("the model failed")
	r := newDrivingRigWith(t, time.Minute, c)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)}); err == nil {
		t.Fatal("the answer tool succeeded")
	}
	if says := sess.saysCopy(); !slices.Equal(says, []string{"Sorry, I could not answer that."}) {
		t.Fatalf("the session spoke %q, want one sorry", says)
	}
}
