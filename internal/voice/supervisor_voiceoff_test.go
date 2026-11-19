package voice

import (
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// With voice.enabled off, Ghost Wispr runs no wake detection and no
// Concierge, and plays only the start sound and the error sound of a failed
// red check (R13). These tests turn the voice off while it is busy; the
// control in each keeps it on. From ReviewPR89's round 2, finding 1.

// TestTurningTheVoiceOffDropsAHeldAnswer fails if an answer held while
// people talk still speaks once the voice is turned off.
func TestTurningTheVoiceOffDropsAHeldAnswer(t *testing.T) {
	for _, turnOff := range []bool{false, true} {
		name := "voice stays on"
		if turnOff {
			name = "voice turned off while people talk"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			r := newRig(t, c)
			c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now().Add(-time.Second)}
			r.waitEvent("voice_answer_ready:sess-1")
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					case <-time.After(30 * time.Millisecond):
						r.heard("people keep talking", false, false)
					}
				}
			}()
			defer func() { close(stop); wg.Wait() }()
			time.Sleep(100 * time.Millisecond)
			if turnOff {
				r.setEnabled(false)
			}
			never(t, "the held answer spoke while people talked", 800*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
			if turnOff {
				// Silence now: the dropped answer must not speak by itself.
				close(stop)
				wg.Wait()
				stop = make(chan struct{})
				never(t, "the dropped answer spoke in the silence", 4*r.s.cfg.SilenceWait, func() bool { return len(r.voice.opened) > 0 })
			}
		})
	}
}

// TestTurningTheVoiceOffHandsNoTurn fails if a call heard just before the
// voice is turned off still reaches the Concierge.
func TestTurningTheVoiceOffHandsNoTurn(t *testing.T) {
	for _, turnOff := range []bool{false, true} {
		name := "voice stays on"
		if turnOff {
			name = "voice turned off before the turn wait ends"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge("fact")
			r := newRig(t, c)
			r.heard("Wispr, what is happening with the budget", false, false)
			if turnOff {
				r.setEnabled(false)
			}
			handed := func() bool {
				c.mu.Lock()
				defer c.mu.Unlock()
				return len(c.turns) > 0
			}
			if turnOff {
				never(t, "a turn reached the Concierge after the voice was turned off", 900*time.Millisecond, handed)
				return
			}
			eventually(t, "the turn reached the Concierge", handed)
		})
	}
}

// TestTurningTheVoiceOffCutsSpeech fails if speech already playing goes on
// once the settings turn the voice off, with nobody speaking.
func TestTurningTheVoiceOffCutsSpeech(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRig(t, c)
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	r.waitSpeaking()
	switched := time.Now()
	r.setEnabled(false)
	r.s.EnabledChanged()
	if at, ok := r.speaker.cancelledAt(audio.InputVoice); !ok || at.Before(switched) {
		t.Fatal("turning the voice off did not cut its speech")
	}
	// The session closes as its exchange's answer goroutine sees the cut.
	eventually(t, "the exchange's session closed", func() bool { _, _, closed := sess.state(); return closed })
}
