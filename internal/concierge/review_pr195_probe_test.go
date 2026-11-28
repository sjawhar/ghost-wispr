package concierge

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestReview195LogUsesConciergesCommittedWords fails if the question log
// keeps words the room took back before the turn the Concierge answered
// ("what about XYZ-42? never mind. what about ABC-12?"): words the room takes
// back are used and sent nowhere, and never stored (#191's rule). The real
// Supervisor and Concierge, with the Concierge's own take-back rule. The
// control has no take-back. (Sol's review of #195 at 48ca91a1.)
func TestReview195LogUsesConciergesCommittedWords(t *testing.T) {
	for _, withdrawn := range []bool{false, true} {
		name := "normal control"
		words := "Wispr, what about ABC-12?"
		if withdrawn {
			name = "take-back before the Concierge"
			words = "Wispr, what about XYZ-42 and the cancelled-words-marker? never mind. what about ABC-12?"
		}
		t.Run(name, func(t *testing.T) {
			r := newR7Rig(t)
			r.s.Close()
			logged := make(chan *voice.Question, 4)
			r.s = voice.NewSupervisor(&voice.SupervisorConfig{
				Voice: func() (voice.Voice, error) { return r.voice, nil }, Concierge: r.c, Speaker: r.speaker,
				WakePhrases: []string{"Wispr"}, Session: r.sessionID, Red: r.isRed, Enabled: func() bool { return true },
				Tap: voice.NewTap(), TurnWait: r7TurnWait, SilenceWait: r7SilenceWait, SoundGap: r7SoundGap,
				TakeBack: TakeBack, Asks: Asks, Question: func(q *voice.Question) { logged <- q },
			})
			t.Cleanup(r.s.Close)
			r.hear(words)
			select {
			case q := <-logged:
				if !strings.Contains(q.Answer, "ABC-12") {
					t.Fatalf("control: the Concierge did not answer the question kept: %+v", q)
				}
				for _, taken := range []string{"XYZ-42", "cancelled-words-marker"} {
					if strings.Contains(q.Heard, taken) || strings.Contains(q.VendorWording, taken) {
						t.Fatalf("the row holds words the room took back (%q): %+v", taken, q)
					}
				}
				if withdrawn && q.Heard != "what about ABC-12?" {
					t.Errorf("heard %q, want the question kept", q.Heard)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("no question was logged")
			}
		})
	}
}
