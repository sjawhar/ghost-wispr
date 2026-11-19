package voice

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// stopBound is how soon every stop must silence the exchange.
const stopBound = 100 * time.Millisecond

// TestEveryStopSilencesTheExchangeWithin100ms fails if a gate closing (a
// pause or a light hold), the Jabra button or shutdown leaves the voice
// speaking, its session open, the chime playing or the Concierge's calls
// running for more than 100 ms.
func TestEveryStopSilencesTheExchangeWithin100ms(t *testing.T) {
	stops := map[string]func(r *rig){
		"pause":      func(r *rig) { r.closeGate() },
		"light hold": func(r *rig) { r.closeGate() },
		"jabra":      func(r *rig) { r.s.Stop("jabra button") },
		"shutdown":   func(r *rig) { r.s.Close() },
	}
	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge("INFRA-1 is in review.")
			c.hold = true
			r := newRig(t, c)
			sess := r.ask("what is happening with the voice issue?")
			waitSaid(t, sess)

			at := time.Now()
			stop(r)

			eventually(t, "the session closed", func() bool { _, _, closed := sess.state(); return closed })
			interrupted, closedAt, _ := sess.state()
			if interrupted.IsZero() || interrupted.Sub(at) > stopBound {
				t.Errorf("Interrupt %v after the stop, want within %v", interrupted.Sub(at), stopBound)
			}
			if closedAt.Sub(at) > stopBound {
				t.Errorf("Close %v after the stop, want within %v", closedAt.Sub(at), stopBound)
			}
			for _, input := range []string{audio.InputVoice, audio.InputChime} {
				cancelled, ok := r.speaker.cancelledAt(input)
				if !ok || cancelled.Sub(at) > stopBound {
					t.Errorf("Cancel(%q) at %v after the stop (cancelled %v), want within %v", input, cancelled.Sub(at), ok, stopBound)
				}
			}
			select {
			case got := <-c.cancelled:
				if got.Sub(at) > stopBound {
					t.Errorf("the Concierge's turn ended %v after the stop, want within %v", got.Sub(at), stopBound)
				}
			case <-time.After(testWait):
				t.Error("the Concierge's turn was not cancelled")
			}
		})
	}
}

// TestAStopEndsTheTapStream fails if the voice's room audio keeps flowing
// after a stop.
func TestAStopEndsTheTapStream(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r := newRig(t, c)
	r.tap.SetRate(16000)
	sess := r.ask("what is happening?")
	r.closeGate()
	eventually(t, "the tap stream closed", func() bool {
		select {
		case _, ok := <-sess.x.Tap:
			return !ok
		default:
			return false
		}
	})
}

// TestWakesFromBeforeAClosingAreDropped fails if a transcript that is a
// Finalize reply, arrived while the gate was closed, or carries the gate's
// closings count from before the latest closing opens an exchange.
func TestWakesFromBeforeAClosingAreDropped(t *testing.T) {
	cases := map[string]func(tr *Transcript){
		"finalize reply": func(tr *Transcript) { tr.FromFinalize = true },
		"gate closed":    func(tr *Transcript) { tr.GateOpen = false },
		"older closings": func(tr *Transcript) { tr.Closings = 0 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, newFakeConcierge("fact"))
			r.closeGate() // closings is now 1
			tr := transcriptOf("Wispr, what is happening?", 0, true, true, 1)
			change(&tr)
			r.s.OnTranscript(&tr)
			never(t, "a dropped wake opened a voice session", 300*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
			if got := r.events.all(); len(got) != 0 {
				t.Errorf("events %v for a dropped wake, want none", got)
			}
		})
	}
}

// TestAWakeChimesAndTheConciergeGetsTheFinalTurn fails if a wake does not
// chime, open a voice, and hand the asker's words after the wake phrase to
// the Concierge once Deepgram marks the turn final, then speak its answer.
func TestAWakeChimesAndTheConciergeGetsTheFinalTurn(t *testing.T) {
	c := newFakeConcierge("INFRA-1 is in review since this morning.")
	r := newRig(t, c)

	r.heard("Wispr,", false, false)
	r.waitEvent("voice_wake:sess-1")
	sess := r.waitSession()
	eventually(t, "the wake chime", func() bool { return r.speaker.written(audio.InputChime) > 0 })

	r.heard("Wispr, what is happening with", false, false)
	r.heard("Wispr, what is happening with the voice issue?", true, false)
	select {
	case turn := <-c.asked:
		t.Fatalf("the Concierge got %q before the turn was final", turn)
	case <-time.After(100 * time.Millisecond):
	}
	r.heard("Thanks.", true, true)

	turn := r.waitTurn()
	if !strings.HasPrefix(turn, "what is happening with the voice issue?") {
		t.Fatalf("turn %q, want the words after the wake phrase", turn)
	}
	r.waitEvent("voice_exchange_started:sess-1")
	if x := c.exchanges[0]; x.SessionID != "sess-1" || x.AskerTurn != turn || x.Tools == nil || x.Out == nil {
		t.Errorf("exchange %+v, want session sess-1, the turn, tools and the voice output", x)
	}
	if got := waitSaid(t, sess); got != "INFRA-1 is in review since this morning." {
		t.Fatalf("said %q", got)
	}
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	eventually(t, "the session closed after the answer", func() bool { _, _, closed := sess.state(); return closed })
}

// TestAWakeWithNoWordsAfterItEnds fails if an exchange opened on "Wispr."
// alone stays open with nobody asking anything.
func TestAWakeWithNoWordsAfterItEnds(t *testing.T) {
	r := newRig(t, newFakeConcierge("fact"))
	r.heard("Wispr.", true, true)
	sess := r.waitSession()
	eventually(t, "the exchange ended", func() bool { _, _, closed := sess.state(); return closed })
	if len(r.concierge.asked) != 0 {
		t.Error("the Concierge got a turn nobody asked")
	}
}

// TestAWakeWaitsForTheQuestionInTheNextUtterance fails if "Wispr." followed
// by the question as its own utterance does not reach the Concierge.
func TestAWakeWaitsForTheQuestionInTheNextUtterance(t *testing.T) {
	r := newRig(t, newFakeConcierge("fact"))
	r.heard("Wispr.", true, true)
	r.waitSession()
	r.heard("What is happening with the voice issue?", true, true)
	if got := r.waitTurn(); got != "What is happening with the voice issue?" {
		t.Fatalf("turn %q", got)
	}
}

// TestBargeIn fails if two words of someone else's within 0.5 s of each
// other while the voice speaks do not stop it, or if a backchannel alone, or
// one word, does.
func TestBargeIn(t *testing.T) {
	cases := []struct {
		heard string
		stops bool
	}{
		{"wait, that's wrong", true},
		{"hold on", true},
		{"mm-hm", false},
		{"yeah okay", false},
		{"wait", false},
	}
	for _, c := range cases {
		t.Run(c.heard, func(t *testing.T) {
			fc := newFakeConcierge("INFRA-1 is in review since this morning, and the holder is live.")
			fc.hold = true
			r := newRig(t, fc)
			sess := r.ask("what is happening?")
			waitSaid(t, sess)
			r.heard(c.heard, false, false)
			if c.stops {
				eventually(t, "a barge-in", func() bool { i, _, _ := sess.state(); return !i.IsZero() })
				if _, ok := r.speaker.cancelledAt(audio.InputVoice); !ok {
					t.Error("barge-in did not cancel the voice input")
				}
				return
			}
			never(t, "a backchannel or one word stopped the voice", 200*time.Millisecond, func() bool { i, _, _ := sess.state(); return !i.IsZero() })
		})
	}
}

// TestWordsFarApartAreNotABargeIn fails if two words more than 0.5 s apart
// stop the voice.
func TestWordsFarApartAreNotABargeIn(t *testing.T) {
	fc := newFakeConcierge("INFRA-1 is in review.")
	fc.hold = true
	r := newRig(t, fc)
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	tr := transcriptOf("wait there", 50, false, false, 0)
	tr.Words[1].Start = tr.Words[0].Start + 0.9
	r.s.OnTranscript(&tr)
	never(t, "words 0.9 s apart stopped the voice", 200*time.Millisecond, func() bool { i, _, _ := sess.state(); return !i.IsZero() })
}

// TestOwnWordsAreNotABargeInOrAWake fails if Wispr's own words, heard back
// while it speaks, stop it or open an exchange; "Wispr, stop" still stops it.
func TestOwnWordsAreNotABargeInOrAWake(t *testing.T) {
	fc := newFakeConcierge("Wispr here: the voice issue is in review since this morning.")
	fc.hold = true
	r := newRig(t, fc)
	sess := r.ask("what is happening?")
	waitSaid(t, sess)

	r.heard("the voice issue is in review since", false, false)
	r.heard("Wispr here: the voice issue is in review", true, true)
	never(t, "Wispr's own words stopped it or woke it", 200*time.Millisecond, func() bool {
		i, _, _ := sess.state()
		return !i.IsZero() || len(r.voice.opened) > 0
	})

	r.heard("Wispr, stop.", false, false)
	eventually(t, "Wispr, stop stopped it", func() bool { _, _, closed := sess.state(); return closed })
}

// TestHeldAnswerSpeaksAfterSilenceWhileRed fails if a holder's answer that
// lands with no stop since its question does not chime, then speak once the
// room has been silent, or speaks while the light is not red.
func TestHeldAnswerSpeaksAfterSilenceWhileRed(t *testing.T) {
	c := newFakeConcierge("I asked the holder.")
	r := newRig(t, c)
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	eventually(t, "the exchange ended", func() bool { _, _, closed := sess.state(); return closed })

	r.setRed(false)
	chimes := r.speaker.written(audio.InputChime)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", IssueKey: "INFRA-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
	never(t, "an answer spoke or chimed while the light was not red", 400*time.Millisecond, func() bool {
		return len(r.voice.opened) > 0 || r.speaker.written(audio.InputChime) > chimes
	})

	r.setRed(true)
	answer := r.waitSession()
	if got := waitSaid(t, answer); got != "It ships tomorrow." {
		t.Fatalf("said %q", got)
	}
	if r.speaker.written(audio.InputChime) <= chimes {
		t.Error("no answer chime before the answer")
	}
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	eventually(t, "the answer recorded as spoken", func() bool { return slices.Equal(c.spokenIDs(), []string{"t1"}) })
}

// TestAnswerAfterAStopWaitsForAnInvitation fails if a holder's answer whose
// question a stop came after speaks by itself, or does not speak on "Wispr,
// go ahead" while red.
func TestAnswerAfterAStopWaitsForAnInvitation(t *testing.T) {
	c := newFakeConcierge("I asked the holder.")
	c.hold = true
	r := newRig(t, c)
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	asked := time.Now()
	r.closeGate()
	eventually(t, "the session closed", func() bool { _, _, closed := sess.state(); return closed })

	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: asked}
	r.waitEvent("voice_answer_ready:sess-1")
	never(t, "an answer spoke with no invitation after a stop", 500*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })

	r.heard("Wispr, go ahead.", true, true)
	answer := r.waitSession()
	if got := waitSaid(t, answer); got != "It ships tomorrow." {
		t.Fatalf("said %q", got)
	}
	if len(c.asked) != 0 {
		t.Error("\"go ahead\" went to the Concierge as a question")
	}
}

// TestGoAheadWhileNotRedSpeaksNothing fails if an invitation heard while the
// light is not red speaks a held answer.
func TestGoAheadWhileNotRedSpeaksNothing(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r := newRig(t, c)
	r.ask("what is happening?")
	r.closeGate()
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow."}
	r.waitEvent("voice_answer_ready:sess-1")
	r.setRed(false)
	r.heard("Wispr, go ahead.", true, true)
	never(t, "an answer spoke while the light was not red", 400*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
}

// TestTheFloorQueuesOtherSpeech fails if WaitFloor does not hold other speech
// while an exchange holds the floor, or does not let it go after.
func TestTheFloorQueuesOtherSpeech(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	if err := r.s.WaitFloor(t.Context()); err != nil {
		t.Fatalf("WaitFloor with no exchange: %v", err)
	}
	sess := r.ask("what is happening?")
	free := make(chan error, 1)
	go func() { free <- r.s.WaitFloor(t.Context()) }()
	select {
	case <-free:
		t.Fatal("WaitFloor returned while an exchange held the floor")
	case <-time.After(100 * time.Millisecond):
	}
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	select {
	case err := <-free:
		if err != nil {
			t.Fatalf("WaitFloor: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("WaitFloor still waits after the exchange ended")
	}
}

// TestAWakePreemptsOtherSpeech fails if a wake leaves other speech (POST
// /api/tts/speak) playing on the voice input.
func TestAWakePreemptsOtherSpeech(t *testing.T) {
	r := newRig(t, newFakeConcierge("fact"))
	pb := r.speaker.Begin(audio.InputVoice)
	if err := pb.Write(t.Context(), make([]byte, 960), audio.AudioFormat{SampleRate: 24000, Channels: 1}); err != nil {
		t.Fatal(err)
	}
	r.heard("Wispr,", false, false)
	r.waitSession()
	if _, ok := r.speaker.cancelledAt(audio.InputVoice); !ok {
		t.Error("the wake left other speech playing")
	}
}
