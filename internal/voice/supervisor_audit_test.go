package voice

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestAnAnswerWaitsWhenAStopCameAfterItsQuestion fails if a holder's answer
// speaks by itself when a stop came after its question, even though a later
// question, asked with no stop since, would speak by itself.
func TestAnAnswerWaitsWhenAStopCameAfterItsQuestion(t *testing.T) {
	c := newFakeConcierge("I asked the holder.")
	r := newRig(t, c)
	first := r.ask("ask the holder about the voice issue")
	waitSaid(t, first)
	asked := time.Now()
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the first exchange ended", func() bool { _, _, closed := first.state(); return closed })

	r.closeGate() // a pause after the first question
	second := r.ask("what is happening?")
	waitSaid(t, second)
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the second exchange ended", func() bool { _, _, closed := second.state(); return closed })

	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: asked}
	r.waitEvent("voice_answer_ready:sess-1")
	never(t, "an answer whose question a stop came after spoke by itself", 700*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
}

// TestAnAnswerWithNoAskedTimeWaitsForAnInvitation fails if an answer that
// does not say when its question was sent speaks by itself.
func TestAnAnswerWithNoAskedTimeWaitsForAnInvitation(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow."}
	r.waitEvent("voice_answer_ready:sess-1")
	never(t, "an answer with no asked time spoke by itself", 700*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
}

// TestAnEndedOrDeletedSessionsAnswersAreDropped fails if a held answer of a
// session that ended or was deleted can still be spoken.
func TestAnEndedOrDeletedSessionsAnswersAreDropped(t *testing.T) {
	for name, forget := range map[string]func(s *Supervisor){
		"ended":   func(s *Supervisor) { s.SessionEnded("sess-1") },
		"deleted": func(s *Supervisor) { s.SessionsDeleted([]string{"sess-1"}) },
	} {
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			r := newRig(t, c)
			c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow."}
			r.waitEvent("voice_answer_ready:sess-1")
			forget(r.s)
			r.heard("Wispr, go ahead.", true, true)
			never(t, "an ended or deleted session's answer spoke", 500*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
		})
	}
}

// TestADeleteForgetsTheRecentTranscript fails if words of a deleted segment
// or session still reach the next exchange as its recent transcript.
func TestADeleteForgetsTheRecentTranscript(t *testing.T) {
	for name, forget := range map[string]func(s *Supervisor){
		"segment": func(s *Supervisor) { s.SegmentsDeleted("sess-1") },
		"session": func(s *Supervisor) { s.SessionsDeleted([]string{"sess-1"}) },
	} {
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge("fact")
			r := newRig(t, c)
			r.heard("the words someone deleted", true, true)
			forget(r.s)
			r.ask("what is happening?")
			for _, turn := range c.exchanges[0].Recent {
				if strings.Contains(turn.Text, "deleted") {
					t.Fatalf("the next exchange's recent transcript holds deleted words: %q", turn.Text)
				}
			}
		})
	}
}

// TestAStopEndsTheAnswerChimeThatBeginsAfterIt fails if the answer chime can
// still play when a stop lands as its playback begins.
func TestAStopEndsTheAnswerChimeThatBeginsAfterIt(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	release := r.speaker.holdBegins(audio.InputChime)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
	r.waitEvent("voice_answer_ready:sess-1")
	time.Sleep(300 * time.Millisecond) // the chime's Begin is held
	r.s.Stop("Jabra button")
	time.Sleep(50 * time.Millisecond)
	release()
	never(t, "the answer chime played after a stop", 400*time.Millisecond, func() bool { return r.speaker.written(audio.InputChime) > 0 })
}

// TestAWakePreemptsSpeechThatHasNotStartedYet fails if a wake leaves other
// speech that began but has not sounded yet free to play over the exchange.
func TestAWakePreemptsSpeechThatHasNotStartedYet(t *testing.T) {
	r := newRig(t, newFakeConcierge("fact"))
	pb := r.speaker.Begin(audio.InputVoice) // POST /api/tts/speak, its first frame not written
	r.heard("Wispr,", false, false)
	r.waitSession()
	eventually(t, "the wake cancelled the voice input", func() bool { _, ok := r.speaker.cancelledAt(audio.InputVoice); return ok })
	if err := pb.Write(t.Context(), make([]byte, 960), audio.AudioFormat{SampleRate: 24000, Channels: 1}); err == nil {
		t.Fatal("other speech still plays after the wake")
	}
}

// TestTheConciergeReadEndsWithTheExchange fails if the context Heard gets
// outlives the exchange: a stop must end the Concierge's read too.
func TestTheConciergeReadEndsWithTheExchange(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r := newRig(t, c)
	r.ask("what is happening?")
	heard := c.heardCalls()
	if len(heard) == 0 {
		t.Fatal("the Concierge heard nothing")
	}
	r.closeGate()
	ctx := heard[0].ctx
	eventually(t, "the read's context ended", func() bool { return ctx.Err() != nil })
}

// TestTheTapsDropsAreLogged fails if frames the tap dropped during an exchange
// go unreported.
func TestTheTapsDropsAreLogged(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	c := newFakeConcierge()
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		v := drivingVoice{r.voice} // a voice that reads room audio
		cfg.Voice = func() (Voice, error) { return v, nil }
		cfg.Logf = func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		}
	})
	r.tap.SetRate(16000)
	r.heard("Wispr, what is happening?", true, true)
	r.waitSession()
	for range 2 * tapQueue {
		if _, err := r.tap.Write(pcmBytes(4000, 0)); err != nil {
			t.Fatal(err)
		}
	}
	r.closeGate()
	eventually(t, "a log line of the tap's drops", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range logs {
			if strings.Contains(l, "dropped") && strings.Contains(l, "tap") {
				return true
			}
		}
		return false
	})
}
