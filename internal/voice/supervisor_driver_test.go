package voice

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// drivingVoice is a voice whose vendor drives the turn, as voices B and C do.
type drivingVoice struct{ *fakeVoice }

func (drivingVoice) DrivesTurn() bool { return true }

func newDrivingRig(t *testing.T, idle time.Duration) *rig {
	t.Helper()
	r := newRigWith(t, newFakeConcierge(), func(c *SupervisorConfig, r *rig) {
		v := drivingVoice{r.voice}
		c.Voice = func() Voice { return v }
		c.IdleWait = idle
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
