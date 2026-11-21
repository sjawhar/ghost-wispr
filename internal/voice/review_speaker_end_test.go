package voice

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// reviewGate holds the speaker's report that the voice's speech ended by
// itself, and every report after it, in order, until release: heldReports,
// which queues them and returns. Sol's round-3 probe blocked inside the
// speech listener; with #133 the Supervisor's FlushSpeech waits for the
// listener's delivery under the speaker's delivery lock, so a listener that
// blocks deadlocks it.
func reviewGate(t *testing.T, cfg *SupervisorConfig, r *rig) (*heldReports, func()) {
	g := holdReports(t, cfg, r)
	cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
	cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
	return g, g.release
}

func reviewWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestReviewDelayedSpeakerEnd(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		for _, end := range []string{"stop", "never mind", "voice off", "vendor error", "vendor end", "delete session", "barge-in"} {
			t.Run(fmt.Sprintf("delayed=%v/%s", delayed, end), func(t *testing.T) {
				var g *heldReports
				var release func()
				r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) { g, release = reviewGate(t, cfg, r) })
				r.heard("Wispr, what is happening?", true, true)
				sess := r.waitSession()
				sess.mu.Lock()
				sess.endLate = time.Second
				sess.mu.Unlock()
				speakAs(t, sess, "The issue is in review since this morning.")
				r.waitSpeaking()
				r.speaker.finish(audio.InputVoice)
				reviewWait(t, g.held, "completed speech queued")
				if !delayed {
					release()
					reviewWait(t, g.delivered, "completed speech delivered")
				}
				switch end {
				case "stop":
					r.s.Stop("review stop")
				case "never mind":
					r.heard("Wispr, never mind.", true, true)
				case "voice off":
					r.setEnabled(false)
					r.s.EnabledChanged()
				case "vendor error":
					sess.fail(errors.New("review vendor error"))
				case "vendor end":
					sess.die()
				case "delete session":
					r.s.SessionsDeleted([]string{"sess-1"})
				case "barge-in":
					r.heard("Wait, what about the other issue?", true, true)
				}
				if end == "barge-in" {
					if delayed {
						eventually(t, "barge-in delivered", func() bool { interrupted, _, _ := sess.state(); return !interrupted.IsZero() })
					}
				} else {
					eventually(t, "exchange closed", func() bool { _, _, closed := sess.state(); return closed })
				}
				release()
				reviewWait(t, g.delivered, "completed speech delivered")
				r.waitEvent("voice_spoken:sess-1")
			})
		}
	}
}

func TestReviewPreviousSpeechIsNotNewExchangeSpoken(t *testing.T) {
	var g *heldReports
	var release func()
	r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) { g, release = reviewGate(t, cfg, r) })
	// Speech outside any exchange, e.g. POST /api/tts/speak, has played.
	pb := r.speaker.Begin(audio.InputVoice, nil)
	if err := pb.Write(t.Context(), make([]byte, 960), audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err != nil {
		t.Fatal(err)
	}
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	reviewWait(t, g.held, "previous speech queued")
	r.heard("Wispr, what is happening?", true, true)
	_ = r.waitSession()
	r.waitEvent("voice_exchange_started:sess-1")
	release()
	reviewWait(t, g.delivered, "previous speech delivered")
	// A real completed-speech report is a positive control for the same
	// event delivery code, not an assertion on a guessed sleep duration.
	r.s.Stop("review stop")
	time.Sleep(50 * time.Millisecond)
	if n := spokenCount(r); n != 0 {
		t.Fatalf("voice_spoken %d times for a new exchange whose voice never spoke; events %v", n, r.events.all())
	}
}
