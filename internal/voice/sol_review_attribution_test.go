package voice

import (
	"testing"
	"time"
)

// Option B keeps words GPT-Live had sent before a cut, though their audio
// never played. The own-word set lives for 30 seconds while Wispr speaks or
// its echo can still be heard, so a matching call or command can be ignored
// too. This is the measured, documented limit; unrelated commands still run.
func TestSolReviewAcceptedAttributionAffectsWakeCommands(t *testing.T) {
	for _, collision := range []bool{false, true} {
		name := "unrelated own words control"
		if collision {
			name = "twenty second old unplayed command words"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			r := newRig(t, c)
			r.s.mu.Lock()
			text := "an unrelated answer"
			if collision {
				text = "Wispr never mind"
			}
			r.s.addOwnLocked(text, time.Now().Add(-20*time.Second))
			r.s.voiceSpeaking = true
			r.s.mu.Unlock()
			r.heard("Wispr, never mind", true, true)
			if !collision {
				eventually(t, "normal cancellation command", func() bool { return len(c.neverMindCalls()) == 1 })
			} else {
				never(t, "a colliding cancellation command was not suppressed", 100*time.Millisecond, func() bool { return len(c.neverMindCalls()) > 0 })
			}
		})
	}
}
