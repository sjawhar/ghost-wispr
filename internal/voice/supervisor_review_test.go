package voice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// TestGoAheadAfterItsOwnInterimSpeaksTheAnswer fails if "Wispr, go ahead",
// whose first interim result "Wispr," opened a wake exchange, does not speak
// the held answer, or sends "go" to the Concierge as a question.
func TestGoAheadAfterItsOwnInterimSpeaksTheAnswer(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow."} // needs an invitation
	r.waitEvent("voice_answer_ready:sess-1")

	r.heard("Wispr,", false, false)
	wake := r.waitSession()
	r.heard("Wispr, go ahead.", true, true)
	r.s.OnUtteranceEnd(0, true, monoclock.Now())
	answer := r.waitSession()
	if got := waitSaid(t, answer); got != "It ships tomorrow." {
		t.Fatalf("said %q, want the held answer", got)
	}
	eventually(t, "the wake exchange ended", func() bool { _, _, closed := wake.state(); return closed })
	select {
	case turn := <-c.asked:
		t.Fatalf("the Concierge was asked %q", turn)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestAWakeTheFinalWithdrawsEndsTheExchange fails if an exchange that an
// interim wake opened goes on, and hands the sentence to the Concierge, once
// the final result of the same words holds no wake.
func TestAWakeTheFinalWithdrawsEndsTheExchange(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	r.heard("Whisper", false, false)
	sess := r.waitSession()
	r.heard("Whisper networks are a problem.", true, true)
	eventually(t, "the misheard wake's exchange ended", func() bool { _, _, closed := sess.state(); return closed })
	select {
	case turn := <-c.asked:
		t.Fatalf("the Concierge was asked %q, a sentence nobody addressed to Wispr", turn)
	case <-time.After(300 * time.Millisecond):
	}
}

// blockingHeard is a Concierge whose Heard waits until released.
type blockingHeard struct {
	*fakeConcierge
	entered chan struct{}
	release chan struct{}
}

func (b *blockingHeard) Heard(ctx context.Context, x *Exchange, words string) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	b.fakeConcierge.Heard(ctx, x, words)
}

// TestAStopDoesNotWaitForTheConciergesHeard fails if a gate closing waits
// while the Concierge's Heard is slow: the pause path must never wait on it.
func TestAStopDoesNotWaitForTheConciergesHeard(t *testing.T) {
	b := &blockingHeard{fakeConcierge: newFakeConcierge(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(b.release) })
	r := newRigWith(t, b.fakeConcierge, func(cfg *SupervisorConfig, _ *rig) { cfg.Concierge = b })
	go r.s.OnTranscript(func() *Transcript { tr := transcriptOf("Wispr, what is happening?", 0, false, false, 0); return &tr }())
	select {
	case <-b.entered:
	case <-time.After(testWait):
		t.Fatal("Heard was never called")
	}
	closed := make(chan struct{})
	go func() { r.s.GateClosed(1); close(closed) }()
	select {
	case <-closed:
	case <-time.After(stopBound):
		t.Fatal("a gate closing waited for the Concierge's Heard")
	}
}

// TestAGateClosingForgetsTheRecentTranscript fails if words heard before a
// pause reach the next exchange as its recent transcript.
func TestAGateClosingForgetsTheRecentTranscript(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	r.heard("words before the pause", true, true)
	r.closeGate()
	r.ask("what is happening?")
	for _, turn := range c.exchanges[0].Recent {
		if turn.Text == "words before the pause" {
			t.Fatal("the next exchange's recent transcript holds words from before the pause")
		}
	}
}

// TestAnAnswerNeedingAnInvitationDoesNotHoldBackTheNext fails if an answer
// that waits for "Wispr, go ahead" keeps a later answer, asked with no stop
// since, from chiming and speaking by itself.
func TestAnAnswerNeedingAnInvitationDoesNotHoldBackTheNext(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "The first answer."} // needs an invitation
	r.waitEvent("voice_answer_ready:sess-1")
	c.answers <- PendingAnswer{TaskID: "t2", SessionID: "sess-1", Text: "The second answer.", AskedAt: time.Now()}
	answer := r.waitSession()
	if got := waitSaid(t, answer); got != "The second answer." {
		t.Fatalf("said %q, want the answer that needs no invitation", got)
	}
}

// TestAHeldAnswerWhoseVoiceFailsToOpenStaysPending fails if an answer whose
// voice could not open is lost instead of waiting for an invitation.
func TestAHeldAnswerWhoseVoiceFailsToOpenStaysPending(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	r.voice.failOpens(errors.New("the vendor refused the connection"))
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
	r.waitEvent("voice_answer_ready:sess-1")
	time.Sleep(700 * time.Millisecond) // the chime, the silence, the failed open
	r.voice.failOpens(nil)
	never(t, "a failed answer spoke again with no invitation", 400*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
	r.heard("Wispr, go ahead.", true, true)
	answer := r.waitSession()
	if got := waitSaid(t, answer); got != "It ships tomorrow." {
		t.Fatalf("said %q", got)
	}
}

// TestANewCallKeepsItsWakeChime fails if the cut of the exchange a new call
// replaces can land after the new wake chime began, and end it.
func TestANewCallKeepsItsWakeChime(t *testing.T) {
	c := newFakeConcierge("A long answer about the voice issue.")
	c.hold = true
	r := newRig(t, c)
	first := r.ask("what is happening?")
	waitSaid(t, first)
	r.speaker.delayCancels(audio.InputChime, 100*time.Millisecond)
	r.speaker.paceWrites(audio.InputChime, 20*time.Millisecond) // the chime plays in real time
	before := r.speaker.written(audio.InputChime)
	r.heard("Wispr, what about the other issue?", false, false)
	r.waitSession()
	frames := (len(WakeChime()) + 959) / 960
	eventually(t, "the whole new wake chime", func() bool { return r.speaker.written(audio.InputChime)-before >= frames })
}

// TestOnlyAVoiceThatDrivesItsTurnGetsRoomAudio fails if voice A, which never
// reads room audio, is given a tap (each buffer copied, then dropped and
// logged as read too slowly), or a voice that drives its own turn is not.
func TestOnlyAVoiceThatDrivesItsTurnGetsRoomAudio(t *testing.T) {
	a := newRig(t, newFakeConcierge("fact"))
	if sess := a.ask("what is happening?"); sess.x.Tap != nil {
		t.Error("voice A got a tap of room audio it never reads")
	}
	b := newDrivingRig(t, time.Minute)
	b.heard("Wispr, what is happening?", true, true)
	if sess := b.waitSession(); sess.x.Tap == nil {
		t.Error("a voice that drives its turn got no room audio")
	}
}

// TestAFinalThatDropsTheNameKeepsTheCall fails if a final result that no
// longer holds the name the interim heard, as Deepgram's live finals sometimes
// drop it ("Wispr, tell me everything …" finalised as "Tell me everything …"),
// ends the exchange instead of handing the question over. The final keeps
// its other words' times, as Deepgram does; the turn is the interim's words
// after the wake, then the final's words that follow them (sinceWakeLocked,
// Sol's reviews of #192), where it was the final's text whole before.
func TestAFinalThatDropsTheNameKeepsTheCall(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	r.heard("Wispr, tell me", false, false)
	r.waitSession()
	final := transcriptOf("Wispr, Tell me everything about the voice issue.", 0, true, true, 0)
	final.Words = final.Words[1:]
	final.Text = "Tell me everything about the voice issue."
	r.s.OnTranscript(&final)
	if got, want := r.waitTurn(), "tell me everything about the voice issue."; got != want {
		t.Fatalf("turn %q, want %q", got, want)
	}
}

// TestAnAnswerOfASessionNoLongerRecordingIsNeverSpoken fails if a held answer
// whose session is not the one recording now (it ended before the Supervisor
// heard, or the answer arrived after the end) chimes or speaks, by itself or
// on "Wispr, go ahead", or is recorded as spoken.
func TestAnAnswerOfASessionNoLongerRecordingIsNeverSpoken(t *testing.T) {
	for name, invite := range map[string]bool{"by itself": false, "on go ahead": true} {
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			r := newRig(t, c)
			r.setSession("sess-2") // sess-1 ended, and sess-2 started
			chimes := r.speaker.written(audio.InputChime)
			c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
			r.waitEvent("voice_answer_ready:sess-1")
			if invite {
				r.heard("Wispr, go ahead.", true, true)
			}
			never(t, "an answer of a session no longer recording chimed or spoke", 700*time.Millisecond, func() bool {
				return len(r.voice.opened) > 0 || r.speaker.written(audio.InputChime) > chimes
			})
			if got := c.spokenIDs(); len(got) != 0 {
				t.Fatalf("recorded as spoken: %v", got)
			}
		})
	}
}
