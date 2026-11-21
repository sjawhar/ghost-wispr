package voice

import (
	"errors"
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

// pendingCount is how many answers the Supervisor holds.
func pendingCount(r *rig) int {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return len(r.s.pending)
}

// TestNeverMindTwiceDropsTwoAnswers fails if two "Wispr, never mind" in a
// row, the second said while the Concierge still answers the first, drop
// only one of two held answers: each drops its own, in order.
func TestNeverMindTwiceDropsTwoAnswers(t *testing.T) {
	c := newFakeConcierge()
	c.neverMindHold = make(chan struct{})
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "One.")
	c.answers <- PendingAnswer{TaskID: "t2", SessionID: "sess-1", Text: "Two."}
	r.waitEvent("voice_answer_ready:sess-1")

	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the first cancel", func() bool { return len(c.neverMindCalls()) == 1 })
	r.heard("Wispr, never mind.", true, true)
	c.neverMindHold <- struct{}{}
	eventually(t, "the second cancel", func() bool { return len(c.neverMindCalls()) == 2 })
	c.neverMindHold <- struct{}{}
	eventually(t, "the waiting light off: both answers dropped", func() bool { return !light.lit() })
	if got := pendingCount(r); got != 0 {
		t.Fatalf("%d answers still held after two never minds, want none", got)
	}
}

// TestTwoNeverMindsCancelTheWorkingQuestionThenDropTheNewestAnswer fails if,
// with one question still working and two answers held, two "Wispr, never
// mind" in a row do anything but cancel that question and then drop the
// newest answer: the older one still waits.
func TestTwoNeverMindsCancelTheWorkingQuestionThenDropTheNewestAnswer(t *testing.T) {
	c := newFakeConcierge()
	c.working = 1
	c.neverMindHold = make(chan struct{})
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "One.")
	c.answers <- PendingAnswer{TaskID: "t2", SessionID: "sess-1", Text: "Two."}
	r.waitEvent("voice_answer_ready:sess-1")

	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the first cancel", func() bool { return len(c.neverMindCalls()) == 1 })
	r.heard("Wispr, never mind.", true, true)
	c.neverMindHold <- struct{}{}
	eventually(t, "the second cancel", func() bool { return len(c.neverMindCalls()) == 2 })
	c.neverMindHold <- struct{}{}
	eventually(t, "one answer dropped", func() bool { return pendingCount(r) == 1 })
	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "One." {
		t.Fatalf("said %q, want the older answer: the second never mind drops the newest", got)
	}
}

// TestAGoAheadWithNoVoiceOutlivesAnOlderNeverMind fails if a "Wispr, go
// ahead" that could not speak, for want of a voice, leaves its answer to an
// older never mind still running: the later word keeps it waiting.
func TestAGoAheadWithNoVoiceOutlivesAnOlderNeverMind(t *testing.T) {
	c := newFakeConcierge()
	c.neverMindHold = make(chan struct{})
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the cancel asked of the Concierge", func() bool { return len(c.neverMindCalls()) == 1 })
	r.mu.Lock()
	r.voiceErr = errors.New("no voice")
	r.mu.Unlock()
	r.heard("Wispr, go ahead.", true, true)

	c.neverMindHold <- struct{}{}
	never(t, "the older never mind dropped the answer", 300*time.Millisecond, func() bool { return pendingCount(r) == 0 || !light.lit() })
	r.mu.Lock()
	r.voiceErr = nil
	r.mu.Unlock()
	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
		t.Fatalf("said %q", got)
	}
}

// TestNeverMindCutsAnAnswerSoundStillPlaying fails if the answer sound,
// queued in full on the speaker but still playing, goes on after "Wispr,
// never mind" drops the last answer.
func TestNeverMindCutsAnAnswerSoundStillPlaying(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	r.speaker.holdDrain(audio.InputChime)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	eventually(t, "the answer sound queued in full", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.chimeDraining
	})
	before := time.Now()
	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the waiting light off", func() bool { return !light.lit() })
	eventually(t, "the answer sound cut", func() bool {
		at, ok := r.speaker.cancelledAt(audio.InputChime)
		return ok && at.After(before)
	})
}

// TestNeverMindCutsAnAnswerSoundThatStartedLate fails if the answer sound
// goes on after "Wispr, never mind" drops the last answer when the speaker
// took long to open: its last frames were queued long after it was
// scheduled, and still play.
func TestNeverMindCutsAnAnswerSoundThatStartedLate(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	r.speaker.holdDrain(audio.InputChime)
	release := r.speaker.holdBegins(audio.InputChime) // the speaker opens late
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	eventually(t, "the answer sound scheduled", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.chimeCancel != nil
	})
	time.Sleep(time.Second)
	release()
	eventually(t, "the answer sound queued in full", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.chimeDraining
	})
	before := time.Now()
	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the waiting light off", func() bool { return !light.lit() })
	eventually(t, "the answer sound cut", func() bool {
		at, ok := r.speaker.cancelledAt(audio.InputChime)
		return ok && at.After(before)
	})
}

// TestNeverMindDropsOnlyAnAnswerHeldWhenItWasSaid fails if a never mind drops
// an answer that was speaking when it was said and that a stop put back to
// wait before the Concierge answered: it drops the newest answer held when
// it was said.
func TestNeverMindDropsOnlyAnAnswerHeldWhenItWasSaid(t *testing.T) {
	c := newFakeConcierge()
	c.neverMindHold = make(chan struct{})
	r, light := newWaitingRig(t, c)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", IssueKey: "ABC-1", Text: "About ABC-1."}
	r.waitEvent("voice_answer_ready:sess-1")
	c.answers <- PendingAnswer{TaskID: "t2", SessionID: "sess-1", IssueKey: "ABC-2", Text: "About ABC-2."}
	r.waitEvent("voice_answer_ready:sess-1")
	eventually(t, "the waiting light on", light.lit)
	r.heard("Wispr, go ahead, ABC two.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "About ABC-2." {
		t.Fatalf("said %q, want ABC-2's answer", got)
	}

	r.heard("Wispr, never mind.", true, true) // ABC-2 speaks; ABC-1 is the newest held
	eventually(t, "the cancel asked of the Concierge", func() bool { return len(c.neverMindCalls()) == 1 })
	r.s.Stop("Jabra button") // within the replay window: ABC-2 waits again
	eventually(t, "both answers waiting", func() bool { return pendingCount(r) == 2 })
	c.neverMindHold <- struct{}{}
	eventually(t, "one answer dropped", func() bool { return pendingCount(r) == 1 })
	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "About ABC-2." {
		t.Fatalf("said %q, want ABC-2's answer: the never mind must drop ABC-1, held when it was said", got)
	}
}

// TestNoNeverMindStartsAfterClose fails if a never mind still queued behind
// another when the Supervisor closes asks the Concierge anything afterwards.
func TestNoNeverMindStartsAfterClose(t *testing.T) {
	c := newFakeConcierge()
	c.neverMindHold = make(chan struct{})
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "One.")
	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the first cancel", func() bool { return len(c.neverMindCalls()) == 1 })
	r.heard("Wispr, never mind.", true, true)
	r.s.Close() // the first call returns with the closed context
	never(t, "a queued never mind asked the Concierge after Close", 300*time.Millisecond, func() bool { return len(c.neverMindCalls()) > 1 })
}

// TestNeverMindLeavesASoundAfterTheAnswerSoundAlone fails if a never mind
// cuts the chime input for an answer sound already cut, as when a
// go-ahead's exchange took the floor: the error sound of that exchange's
// failed open plays on.
func TestNeverMindLeavesASoundAfterTheAnswerSoundAlone(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	eventually(t, "the answer sound queued in full", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.speaker.written(audio.InputChime) > 0 && r.s.chimeCancel == nil
	})
	r.voice.failOpens(errors.New("the vendor refused the connection"))
	r.heard("Wispr, go ahead.", true, true) // its open fails: the error sound, and the answer waits again
	eventually(t, "the answer waiting again", func() bool { return pendingCount(r) == 1 && light.lit() })
	before := time.Now()
	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the waiting light off", func() bool { return !light.lit() })
	if at, ok := r.speaker.cancelledAt(audio.InputChime); ok && at.After(before) {
		t.Fatal("the never mind cut the chime input after the answer sound was already cut")
	}
}

// TestALaterGoAheadOutlivesAnOlderNeverMind fails if a "Wispr, never mind"
// whose Concierge call is still running drops an answer that a later "Wispr,
// go ahead" spoke and a stop then put back to wait: the later word wins.
func TestALaterGoAheadOutlivesAnOlderNeverMind(t *testing.T) {
	c := newFakeConcierge()
	c.neverMindHold = make(chan struct{})
	r, light := newWaitingRig(t, c)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")

	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the cancel asked of the Concierge", func() bool { return len(c.neverMindCalls()) == 1 })
	r.heard("Wispr, go ahead.", true, true)
	waitSaid(t, r.waitSession())
	r.s.Stop("Jabra button") // within the replay window: it waits again
	eventually(t, "the answer waiting again", func() bool { return pendingCount(r) == 1 && light.lit() })

	c.neverMindHold <- struct{}{}
	never(t, "the older never mind dropped the answer", 300*time.Millisecond, func() bool { return !light.lit() || pendingCount(r) == 0 })
	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
		t.Fatalf("said %q, want the answer the older never mind did not drop", got)
	}
}

// TestNeverMindDropsTheNewestArrival fails if "Wispr, never mind" drops an
// answer other than the session's newest by arrival: an answer cut early goes
// back to the front of the queue, and stays the newest.
func TestNeverMindDropsTheNewestArrival(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", IssueKey: "ABC-1", Text: "About ABC-1."}
	r.waitEvent("voice_answer_ready:sess-1")
	c.answers <- PendingAnswer{TaskID: "t2", SessionID: "sess-1", IssueKey: "ABC-2", Text: "About ABC-2."}
	r.waitEvent("voice_answer_ready:sess-1")
	eventually(t, "the waiting light on", light.lit)
	r.heard("Wispr, go ahead, ABC two.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "About ABC-2." {
		t.Fatalf("said %q, want ABC-2's answer", got)
	}
	r.s.Stop("Jabra button") // within the replay window: ABC-2 waits again, at the front
	eventually(t, "both answers waiting", func() bool { return pendingCount(r) == 2 })

	r.heard("Wispr, never mind.", true, true)
	eventually(t, "one answer dropped", func() bool { return pendingCount(r) == 1 })
	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "About ABC-1." {
		t.Fatalf("said %q after never mind, want ABC-1's answer: never mind must drop the newest arrival, ABC-2's", got)
	}
}

// TestAFailedNeverMindLeavesTheAnswerWaitingForAnInvitation fails if an
// answer that "Wispr, never mind" meant to drop, whose Concierge call failed,
// speaks by itself: it waits for "Wispr, go ahead" instead.
func TestAFailedNeverMindLeavesTheAnswerWaitingForAnInvitation(t *testing.T) {
	c := newFakeConcierge()
	c.neverMindErr = errors.New("database is locked")
	r, light := newWaitingRig(t, c)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()} // needs no invitation
	r.waitEvent("voice_answer_ready:sess-1")
	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the cancel asked of the Concierge", func() bool { return len(c.neverMindCalls()) == 1 })
	never(t, "the answer spoke by itself after a failed never mind", 800*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
	if !light.lit() {
		t.Fatal("the waiting light went off with the answer still waiting")
	}
	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
		t.Fatalf("said %q on invitation", got)
	}
}

// TestNeverMindStopsAQueuedAnswerSound fails if the answer sound of an answer
// that "Wispr, never mind" dropped plays after the drop, when it was queued
// before.
func TestNeverMindStopsAQueuedAnswerSound(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	release := r.speaker.holdBegins(audio.InputChime)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	eventually(t, "the answer sound queued", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.chimeCancel != nil
	})

	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the waiting light off", func() bool { return !light.lit() })
	release()
	never(t, "the dropped answer's sound played", 400*time.Millisecond, func() bool { return r.speaker.written(audio.InputChime) > 0 })
}

// TestAnAnswerArrivingWhileTheVoiceIsOffNeitherFlashesNorSpeaks fails if an
// answer that lands after the voice was turned off lights the waiting light,
// or speaks once the voice is on again.
func TestAnAnswerArrivingWhileTheVoiceIsOffNeitherFlashesNorSpeaks(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	r.setEnabled(false)
	r.s.EnabledChanged()
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
	never(t, "the waiting light on for an answer while the voice is off", 400*time.Millisecond, light.lit)

	r.setEnabled(true)
	r.s.EnabledChanged()
	r.heard("Wispr, go ahead.", true, true)
	never(t, "an answer from while the voice was off spoke", 500*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
}
