package voice

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// fakeWaitingLight records each On and Off.
type fakeWaitingLight struct {
	mu    sync.Mutex
	calls []string
}

func (l *fakeWaitingLight) On()  { l.record("on") }
func (l *fakeWaitingLight) Off() { l.record("off") }

func (l *fakeWaitingLight) record(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

// lit reports whether the light's last call was On.
func (l *fakeWaitingLight) lit() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls) > 0 && l.calls[len(l.calls)-1] == "on"
}

func (l *fakeWaitingLight) callsCopy() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// newWaitingRig is newRig with a waiting light.
func newWaitingRig(t *testing.T, c *fakeConcierge) (*rig, *fakeWaitingLight) {
	t.Helper()
	light := &fakeWaitingLight{}
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.WaitingLight = light })
	return r, light
}

// holdAnswer hands the rig an answer that waits for "Wispr, go ahead" (it
// says no asked time), and waits until it is held and the light is on.
func holdAnswer(t *testing.T, r *rig, light *fakeWaitingLight, taskID, text string) {
	t.Helper()
	r.concierge.answers <- PendingAnswer{TaskID: taskID, SessionID: "sess-1", Text: text}
	r.waitEvent("voice_answer_ready:sess-1")
	eventually(t, "the waiting light on for the held answer", light.lit)
}

// TestTheWaitingLightShowsAHeldAnswerUntilItIsSpoken fails if the waiting
// light does not come on when an answer is held, or does not go off once
// "Wispr, go ahead" speaks it; or if it is told On or Off more than once for
// one change.
func TestTheWaitingLightShowsAHeldAnswerUntilItIsSpoken(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")

	r.heard("Wispr, go ahead.", true, true)
	answer := r.waitSession()
	if got := waitSaid(t, answer); got != "It ships tomorrow." {
		t.Fatalf("said %q", got)
	}
	eventually(t, "the waiting light off once the answer speaks", func() bool { return !light.lit() })
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the answer recorded as spoken", func() bool { return len(c.spokenIDs()) == 1 })
	if got := light.callsCopy(); !slices.Equal(got, []string{"on", "off"}) {
		t.Fatalf("waiting light calls = %v, want on, off", got)
	}
}

// TestTheWaitingLightComesBackWhenASpokenAnswerIsCutEarly fails if a held
// answer that a stop cuts within the replay window, which waits again for
// "Wispr, go ahead", does not light the waiting light again.
func TestTheWaitingLightComesBackWhenASpokenAnswerIsCutEarly(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	r.heard("Wispr, go ahead.", true, true)
	waitSaid(t, r.waitSession())
	eventually(t, "the waiting light off while the answer speaks", func() bool { return !light.lit() })

	r.s.Stop("Jabra button") // within the rig's 300 ms replay window
	eventually(t, "the waiting light on for the answer cut early", light.lit)
}

// TestTheWaitingLightGoesOffWhenNothingWaits fails if the waiting light stays
// on once its held answer can no longer be spoken: its session ended (a pause
// ends it) or was deleted, another session records, the voice was turned off,
// or the Supervisor closed.
func TestTheWaitingLightGoesOffWhenNothingWaits(t *testing.T) {
	for name, end := range map[string]func(r *rig){
		"a pause, which ends the session": func(r *rig) { r.closeGate(); r.s.SessionEnded("sess-1") },
		"the session ended":               func(r *rig) { r.s.SessionEnded("sess-1") },
		"the session deleted":             func(r *rig) { r.s.SessionsDeleted([]string{"sess-1"}) },
		"another session records": func(r *rig) {
			r.mu.Lock()
			r.session = "sess-2"
			r.mu.Unlock()
		},
		"the voice turned off":  func(r *rig) { r.setEnabled(false); r.s.EnabledChanged() },
		"the Supervisor closed": func(r *rig) { r.s.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			r, light := newWaitingRig(t, c)
			holdAnswer(t, r, light, "t1", "It ships tomorrow.")
			end(r)
			eventually(t, "the waiting light off", func() bool { return !light.lit() })
		})
	}
}

// TestNeverMindDropsTheNewestHeldAnswer fails if "Wispr, never mind", with no
// working question to cancel, leaves the newest held answer of the session
// waiting or the waiting light on, or if that answer sounds or speaks
// afterwards, also on "Wispr, go ahead".
func TestNeverMindDropsTheNewestHeldAnswer(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	eventually(t, "the answer sound", func() bool { return slices.Contains(r.speaker.soundsPlayed(), "answer") })
	sounds := len(r.speaker.soundsPlayed())

	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the cancel asked of the Concierge", func() bool { return slices.Equal(c.neverMindCalls(), []string{"sess-1"}) })
	eventually(t, "the waiting light off", func() bool { return !light.lit() })

	r.heard("Wispr, go ahead.", true, true)
	never(t, "a dropped answer spoke or sounded", 500*time.Millisecond, func() bool {
		return len(r.voice.opened) > 0 || len(r.speaker.soundsPlayed()) > sounds
	})
	if got := c.spokenIDs(); len(got) != 0 {
		t.Fatalf("a dropped answer was recorded as spoken: %v", got)
	}
}

// TestNeverMindWithAWorkingQuestionKeepsTheHeldAnswer fails if "Wispr, never
// mind", when the Concierge cancels a working question, also drops an older
// held answer or turns the waiting light off: that answer still waits, and
// "Wispr, go ahead" speaks it.
func TestNeverMindWithAWorkingQuestionKeepsTheHeldAnswer(t *testing.T) {
	c := newFakeConcierge()
	c.working = 1
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")

	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the cancel asked of the Concierge", func() bool { return slices.Equal(c.neverMindCalls(), []string{"sess-1"}) })
	never(t, "the waiting light went off with the held answer still waiting", 300*time.Millisecond, func() bool { return !light.lit() })

	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
		t.Fatalf("said %q, want the older held answer", got)
	}
}
